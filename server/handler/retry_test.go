// Copyright 2026 Palantir Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handler

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "Client.Timeout exceeded while awaiting headers" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func statusError(code int) error {
	return &github.ErrorResponse{Response: &http.Response{StatusCode: code, Request: &http.Request{Method: "POST", URL: &url.URL{}}}}
}

func TestIsTransient(t *testing.T) {
	timeout := &url.Error{Op: "Post", URL: "https://api.github.com/graphql", Err: timeoutError{}}

	tests := map[string]struct {
		Err      error
		Expected bool
	}{
		"nil":               {Err: nil, Expected: false},
		"plain":             {Err: errors.New("invalid policy"), Expected: false},
		"clientTimeout":     {Err: errors.Wrap(timeout, "failed to load pull request details"), Expected: true},
		"deadline":          {Err: errors.Wrap(context.DeadlineExceeded, "failed to read file"), Expected: true},
		"serverError":       {Err: errors.WithStack(statusError(500)), Expected: true},
		"notFound":          {Err: errors.WithStack(statusError(404)), Expected: false},
		"graphqlBadGateway": {Err: errors.New("non-200 OK status code: 502 Bad Gateway body: \"\""), Expected: true},
		"joinedWithOneTransient": {
			Err:      errors.Wrapf(stderrors.Join(errors.New("invalid policy"), timeout), "failed to evaluate %d pull requests", 2),
			Expected: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.Expected, IsTransient(test.Err))
		})
	}
}

type flakyHandler struct {
	mu       sync.Mutex
	failures []error
	calls    int
}

func (h *flakyHandler) Handles() []string { return []string{"pull_request"} }

func (h *flakyHandler) Handle(context.Context, string, string, []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if len(h.failures) == 0 {
		return nil
	}
	err := h.failures[0]
	h.failures = h.failures[1:]
	return err
}

func (h *flakyHandler) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func retryWith(inner *flakyHandler, delays int, maxPending int) *retryingHandler {
	r := NewRetrier(make([]time.Duration, delays), maxPending)(inner).(*retryingHandler)
	r.sleep = func(time.Duration) {}
	return r
}

func waitForRetries(t *testing.T, r *retryingHandler) {
	assert.Eventually(t, func() bool { return len(r.pending) == 0 }, time.Second, time.Millisecond)
}

func TestRetryingHandler(t *testing.T) {
	timeout := errors.WithStack(context.DeadlineExceeded)

	t.Run("retriesUntilItSucceeds", func(t *testing.T) {
		inner := &flakyHandler{failures: []error{timeout, timeout}}
		r := retryWith(inner, 4, 10)

		assert.Error(t, r.Handle(context.Background(), "pull_request", "1", nil))
		waitForRetries(t, r)
		assert.Equal(t, 3, inner.Calls())
	})

	t.Run("stopsAfterTheLastDelay", func(t *testing.T) {
		inner := &flakyHandler{failures: []error{timeout, timeout, timeout, timeout}}
		r := retryWith(inner, 2, 10)

		assert.Error(t, r.Handle(context.Background(), "pull_request", "1", nil))
		waitForRetries(t, r)
		assert.Equal(t, 3, inner.Calls())
	})

	t.Run("doesNotRetryAPermanentError", func(t *testing.T) {
		inner := &flakyHandler{failures: []error{errors.New("invalid policy")}}
		r := retryWith(inner, 4, 10)

		assert.Error(t, r.Handle(context.Background(), "pull_request", "1", nil))
		waitForRetries(t, r)
		assert.Equal(t, 1, inner.Calls())
	})

	t.Run("dropsWhenTooManyAreWaiting", func(t *testing.T) {
		inner := &flakyHandler{failures: []error{timeout}}
		r := retryWith(inner, 4, 1)
		r.pending <- struct{}{}

		assert.Error(t, r.Handle(context.Background(), "pull_request", "1", nil))
		<-r.pending
		assert.Equal(t, 1, inner.Calls())
	})
}
