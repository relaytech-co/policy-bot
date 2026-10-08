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
	"errors"
	"net"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/palantir/go-githubapp/githubapp"
	"github.com/rs/zerolog"
)

// DefaultRetryDelays are the waits before each retry of an event whose handling failed on a transient GitHub error
var DefaultRetryDelays = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute}

// DefaultMaxPendingRetries caps the events waiting to be retried, so an outage cannot grow them without bound
const DefaultMaxPendingRetries = 200

// IsTransient reports whether err is a GitHub failure that may succeed if tried again: a timeout, a 5xx or a rate limit.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return true
	}
	if ghErr, ok := errors.AsType[*github.ErrorResponse](err); ok && ghErr.Response != nil && ghErr.Response.StatusCode >= 500 {
		return true
	}
	if _, ok := errors.AsType[*github.RateLimitError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*github.AbuseRateLimitError](err); ok {
		return true
	}
	// The GraphQL client reports a bad status as text rather than as a typed error
	return strings.Contains(err.Error(), "non-200 OK status code: 5")
}

// NewRetrier returns a wrapper that makes an event handler handle an event again after each of delays when it fails on a transient error.
// Without it the event is dropped, and the status it should have posted stays as it was until the next event.
// At most maxPending events wait to be retried across every handler it wraps.
func NewRetrier(delays []time.Duration, maxPending int) func(githubapp.EventHandler) githubapp.EventHandler {
	pending := make(chan struct{}, maxPending)
	return func(h githubapp.EventHandler) githubapp.EventHandler {
		return &retryingHandler{EventHandler: h, delays: delays, pending: pending}
	}
}

type retryingHandler struct {
	githubapp.EventHandler
	delays  []time.Duration
	pending chan struct{}
	sleep   func(time.Duration)
}

func (h *retryingHandler) Handle(ctx context.Context, eventType, deliveryID string, payload []byte) error {
	err := h.EventHandler.Handle(ctx, eventType, deliveryID, payload)
	if !IsTransient(err) || len(h.delays) == 0 {
		return err
	}

	logger := zerolog.Ctx(ctx)
	select {
	case h.pending <- struct{}{}:
	default:
		logger.Warn().Err(err).Msg("Too many events waiting to be retried, dropping this one")
		return err
	}

	retryCtx := context.WithoutCancel(ctx)
	go func() {
		defer func() { <-h.pending }()
		h.retry(retryCtx, eventType, deliveryID, payload)
	}()
	return err
}

func (h *retryingHandler) retry(ctx context.Context, eventType, deliveryID string, payload []byte) {
	logger := zerolog.Ctx(ctx)
	sleep := h.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	var err error
	for attempt, delay := range h.delays {
		sleep(delay)
		err = h.EventHandler.Handle(ctx, eventType, deliveryID, payload)
		if err == nil {
			logger.Info().Int("attempt", attempt+1).Msg("Retried event handled")
			return
		}
		if !IsTransient(err) {
			break
		}
	}
	logger.Error().Err(err).Msg("Giving up retrying event")
}
