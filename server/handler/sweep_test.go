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
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/palantir/go-githubapp/githubapp"
	"github.com/palantir/policy-bot/pull"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/assert"
)

var sweepNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type prOption func(*SweepPR)

func status(state githubv4.StatusState, age time.Duration) prOption {
	return func(pr *SweepPR) {
		c := &pr.Commits.Nodes[0].Commit
		c.Status = &struct {
			Contexts []struct {
				Context   string
				State     githubv4.StatusState
				CreatedAt time.Time
			}
		}{}
		c.Status.Contexts = append(c.Status.Contexts, struct {
			Context   string
			State     githubv4.StatusState
			CreatedAt time.Time
		}{Context: "relay-checks", State: state, CreatedAt: sweepNow.Add(-age)})
	}
}

func suite(app string, state githubv4.CheckStatusState) prOption {
	return func(pr *SweepPR) {
		c := &pr.Commits.Nodes[0].Commit
		c.CheckSuites.Nodes = append(c.CheckSuites.Nodes, struct {
			Status githubv4.CheckStatusState
			App    *struct{ Slug string }
		}{Status: state, App: &struct{ Slug string }{Slug: app}})
	}
}

func makeSweepPR(number int, updated time.Duration, opts ...prOption) SweepPR {
	pr := SweepPR{Number: number, Mergeable: githubv4.MergeableStateMergeable, UpdatedAt: sweepNow.Add(-updated), BaseRefName: "dev"}
	pr.Commits.Nodes = make([]struct {
		Commit struct {
			Oid    string
			Status *struct {
				Contexts []struct {
					Context   string
					State     githubv4.StatusState
					CreatedAt time.Time
				}
			}
			CheckSuites struct {
				Nodes []struct {
					Status githubv4.CheckStatusState
					App    *struct{ Slug string }
				}
			} `graphql:"checkSuites(first: 50, filterBy: {appId: 15368})"`
		}
	}, 1)
	pr.Commits.Nodes[0].Commit.Oid = "sha" + string(rune('a'+number%26))
	for _, opt := range opts {
		opt(&pr)
	}
	return pr
}

func TestStuckReason(t *testing.T) {
	tests := map[string]struct {
		PR       SweepPR
		Expected string
	}{
		"noStatus":              {PR: makeSweepPR(1, 0), Expected: "no status"},
		"errorStatus":           {PR: makeSweepPR(1, 0, status(githubv4.StatusStateError, time.Hour)), Expected: "error status"},
		"success":               {PR: makeSweepPR(1, 0, status(githubv4.StatusStateSuccess, time.Hour))},
		"failure":               {PR: makeSweepPR(1, 0, status(githubv4.StatusStateFailure, time.Hour))},
		"freshPending":          {PR: makeSweepPR(1, 0, status(githubv4.StatusStatePending, 10*time.Second))},
		"pendingWhileRunning":   {PR: makeSweepPR(1, 0, status(githubv4.StatusStatePending, time.Hour), suite("github-actions", githubv4.CheckStatusStateInProgress))},
		"pendingAfterWorkflows": {PR: makeSweepPR(1, 0, status(githubv4.StatusStatePending, time.Hour), suite("github-actions", githubv4.CheckStatusStateCompleted)), Expected: "pending with no workflow running"},
		"otherAppQueuedForever": {PR: makeSweepPR(1, 0, status(githubv4.StatusStatePending, time.Hour), suite("some-app", githubv4.CheckStatusStateQueued)), Expected: "pending with no workflow running"},
		"draft": {PR: func() SweepPR {
			pr := makeSweepPR(1, 0)
			pr.IsDraft = true
			return pr
		}()},
		"conflicting": {PR: func() SweepPR {
			pr := makeSweepPR(1, 0)
			pr.Mergeable = githubv4.MergeableStateConflicting
			return pr
		}()},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.Expected, stuckReason(&test.PR, "relay-checks", sweepNow, time.Minute))
		})
	}
}

type fakeInstallations struct{ githubapp.InstallationsService }

func (fakeInstallations) GetByRepository(context.Context, string, string) (githubapp.Installation, error) {
	return githubapp.Installation{ID: 42}, nil
}

// fakePager serves pages of pull requests, already sorted most recently updated first
type fakePager struct {
	pages   [][]SweepPR
	cursors []string
}

func (p *fakePager) Page(_ context.Context, _ int64, _, _ string, cursor string) ([]SweepPR, string, error) {
	p.cursors = append(p.cursors, cursor)
	i := 0
	if cursor != "" {
		i = int(cursor[0] - '0')
	}
	next := ""
	if i+1 < len(p.pages) {
		next = string(rune('0' + i + 1))
	}
	return p.pages[i], next, nil
}

func newTestSweeper(pager *fakePager, max int) (*Sweeper, func() []int) {
	var mu sync.Mutex
	var evaluated []int
	s := &Sweeper{
		Base:   Base{Installations: fakeInstallations{}, PullOpts: &PullEvaluationOptions{StatusCheckContext: "relay-checks", IncludeBranchInStatusContext: new(false)}},
		Config: SweepConfig{Repositories: []string{"relaytech-co/relaycode"}, Window: time.Hour, PendingAge: time.Minute, MaxEvaluations: max},
		Pager:  pager.Page,
		now:    func() time.Time { return sweepNow },
		evaluatePR: func(_ context.Context, id int64, loc pull.Locator) error {
			mu.Lock()
			defer mu.Unlock()
			evaluated = append(evaluated, loc.Number)
			return nil
		},
	}
	return s, func() []int {
		mu.Lock()
		defer mu.Unlock()
		out := append([]int(nil), evaluated...)
		evaluated = nil
		sort.Ints(out)
		return out
	}
}

func TestSweeperPass(t *testing.T) {
	ok := status(githubv4.StatusStateSuccess, time.Hour)
	pager := &fakePager{pages: [][]SweepPR{
		{makeSweepPR(1, time.Minute), makeSweepPR(2, 2*time.Minute, ok), makeSweepPR(3, 2*time.Hour)},
		{makeSweepPR(4, 3*time.Hour), makeSweepPR(5, 4*time.Hour, ok)},
		{makeSweepPR(6, 5*time.Hour)},
	}}
	s, evaluated := newTestSweeper(pager, 10)

	// The recent read stops at the page that leaves the window, then the backlog reads its first page
	s.Pass(context.Background())
	assert.Equal(t, []int{1, 3}, evaluated())
	assert.Equal(t, []string{"", ""}, pager.cursors)

	// The next pass's backlog page is the one after, and wraps around after the last
	pager.cursors = nil
	s.Pass(context.Background())
	assert.Equal(t, []int{1, 3, 4}, evaluated())
	assert.Equal(t, []string{"", "1"}, pager.cursors)

	pager.cursors = nil
	s.Pass(context.Background())
	assert.Equal(t, []int{1, 3, 6}, evaluated())
	assert.Equal(t, []string{"", "2"}, pager.cursors)

	// 1 and 3 have used their attempts, and the backlog has wrapped round to the page the recent read already covered
	pager.cursors = nil
	s.Pass(context.Background())
	assert.Empty(t, evaluated())
	assert.Equal(t, []string{"", ""}, pager.cursors)
}

func TestSweeperPassCapsEvaluations(t *testing.T) {
	pager := &fakePager{pages: [][]SweepPR{{makeSweepPR(1, time.Minute), makeSweepPR(2, time.Minute), makeSweepPR(3, time.Minute)}}}
	s, evaluated := newTestSweeper(pager, 2)

	s.Pass(context.Background())
	assert.Equal(t, []int{1, 2}, evaluated(), "recent pull requests first, up to the cap")

	s.Pass(context.Background())
	assert.Equal(t, []int{1, 3}, evaluated(), "the one cut last time goes first, then the most recent")
}

func TestSweepConfigFromEnv(t *testing.T) {
	t.Setenv("SW_REPOSITORIES", "relaytech-co/relaycode, relaytech-co/other")
	t.Setenv("SW_INTERVAL", "90s")
	t.Setenv("SW_MAX_EVALUATIONS", "5")

	var c SweepConfig
	c.SetValuesFromEnv("SW_")

	assert.Equal(t, []string{"relaytech-co/relaycode", "relaytech-co/other"}, c.Repositories)
	assert.Equal(t, 90*time.Second, c.Interval)
	assert.Equal(t, 5, c.MaxEvaluations)
	assert.Equal(t, DefaultSweepWindow, c.Window)
	assert.Equal(t, DefaultSweepPendingAge, c.PendingAge)
}
