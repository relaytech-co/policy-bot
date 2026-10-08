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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/pull"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/shurcooL/githubv4"
)

const (
	DefaultSweepInterval       = 2 * time.Minute
	DefaultSweepWindow         = 2 * time.Hour
	DefaultSweepPendingAge     = time.Minute
	DefaultSweepMaxEvaluations = 10

	// sweepPageSize is how many pull requests one query reads
	sweepPageSize = 50
	// sweepMaxAttempts is how often one head commit is evaluated by the sweep, so a pull request that is pending for good reason is not evaluated every pass
	sweepMaxAttempts = 3
	sweepConcurrency = 5
)

// SweepConfig enables re-evaluating open pull requests whose status looks stuck, because a webhook was missed or its evaluation failed.
type SweepConfig struct {
	// Repositories to sweep, as owner/name. The sweep is off when empty.
	Repositories []string `yaml:"repositories"`
	// Interval between passes.
	Interval time.Duration `yaml:"interval"`
	// Window is how far back each pass reads recently updated pull requests. Each pass also reads one page of older ones, so every open pull request is reached in turn.
	Window time.Duration `yaml:"window"`
	// PendingAge is how long a pending status must be old, with no workflow running, before it counts as stuck.
	PendingAge time.Duration `yaml:"pending_age"`
	// MaxEvaluations caps the evaluations in one pass, recent pull requests first.
	MaxEvaluations int `yaml:"max_evaluations"`
}

func (c *SweepConfig) SetValuesFromEnv(prefix string) {
	if v, ok := os.LookupEnv(prefix + "REPOSITORIES"); ok {
		c.Repositories = nil
		for _, r := range strings.Split(v, ",") {
			if r = strings.TrimSpace(r); r != "" {
				c.Repositories = append(c.Repositories, r)
			}
		}
	}
	setDurationFromEnv(prefix+"INTERVAL", &c.Interval)
	setDurationFromEnv(prefix+"WINDOW", &c.Window)
	setDurationFromEnv(prefix+"PENDING_AGE", &c.PendingAge)
	if v, ok := os.LookupEnv(prefix + "MAX_EVALUATIONS"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxEvaluations = n
		}
	}
	c.fillDefaults()
}

func (c *SweepConfig) fillDefaults() {
	if c.Interval <= 0 {
		c.Interval = DefaultSweepInterval
	}
	if c.Window <= 0 {
		c.Window = DefaultSweepWindow
	}
	if c.PendingAge <= 0 {
		c.PendingAge = DefaultSweepPendingAge
	}
	if c.MaxEvaluations <= 0 {
		c.MaxEvaluations = DefaultSweepMaxEvaluations
	}
}

func setDurationFromEnv(key string, d *time.Duration) {
	if v, ok := os.LookupEnv(key); ok {
		if parsed, err := time.ParseDuration(v); err == nil {
			*d = parsed
		}
	}
}

// SweepPR is the part of an open pull request the sweep decides on.
type SweepPR struct {
	Number      int
	IsDraft     bool
	Mergeable   githubv4.MergeableState
	UpdatedAt   time.Time
	BaseRefName string
	Commits     struct {
		Nodes []struct {
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
						App    *struct {
							Slug string
						}
					}
				} `graphql:"checkSuites(first: 100)"`
			}
		}
	} `graphql:"commits(last: 1)"`
}

func (pr *SweepPR) headSHA() string {
	if len(pr.Commits.Nodes) == 0 {
		return ""
	}
	return pr.Commits.Nodes[0].Commit.Oid
}

// stuckReason says why the pull request's status needs evaluating, or "" when it does not.
func stuckReason(pr *SweepPR, statusContext string, now time.Time, pendingAge time.Duration) string {
	// A draft is evaluated when it is marked ready, and a conflicting pull request runs no workflows until it is fixed
	if pr.IsDraft || pr.Mergeable == githubv4.MergeableStateConflicting || len(pr.Commits.Nodes) == 0 {
		return ""
	}
	commit := pr.Commits.Nodes[0].Commit

	var state githubv4.StatusState
	var createdAt time.Time
	if commit.Status != nil {
		for _, c := range commit.Status.Contexts {
			if c.Context == statusContext {
				state, createdAt = c.State, c.CreatedAt
			}
		}
	}

	switch state {
	case "":
		return "no status"
	case githubv4.StatusStateError:
		return "error status"
	case githubv4.StatusStatePending:
		if now.Sub(createdAt) < pendingAge {
			return ""
		}
		// Only GitHub Actions suites, since other apps can leave a suite queued that never runs
		for _, suite := range commit.CheckSuites.Nodes {
			if suite.App != nil && suite.App.Slug == "github-actions" && suite.Status != githubv4.CheckStatusStateCompleted {
				return ""
			}
		}
		return "pending with no workflow running"
	default:
		return ""
	}
}

// PullRequestPager reads one page of a repository's open pull requests, most recently updated first.
type PullRequestPager func(ctx context.Context, installationID int64, owner, repo, cursor string) (prs []SweepPR, next string, err error)

// Sweeper periodically re-evaluates open pull requests whose status looks stuck.
// Webhooks are the only other trigger, so a missed one or a failed evaluation otherwise leaves a status as it was until the pull request changes.
type Sweeper struct {
	Base
	Config SweepConfig
	Pager  PullRequestPager

	now func() time.Time
	// evaluatePR is Evaluate, replaced in tests
	evaluatePR func(ctx context.Context, installationID int64, loc pull.Locator) error

	mu       sync.Mutex
	attempts map[string]sweepAttempt
	// backlog is the cursor into each repository's older pull requests, read a page a pass
	backlog map[string]string
}

type sweepAttempt struct {
	count int
	at    time.Time
}

type sweepCandidate struct {
	owner, repo string
	number      int
	sha         string
	reason      string
}

// Start runs passes every Config.Interval until ctx is done. It does nothing when no repository is configured.
func (s *Sweeper) Start(ctx context.Context) {
	if len(s.Config.Repositories) == 0 {
		return
	}
	logger := zerolog.Ctx(ctx)
	logger.Info().Strs("repositories", s.Config.Repositories).Msgf("Sweeping stuck pull requests every %s", s.Config.Interval)
	go func() {
		ticker := time.NewTicker(s.Config.Interval)
		defer ticker.Stop()
		for {
			s.Pass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Pass sweeps each configured repository once.
func (s *Sweeper) Pass(ctx context.Context) {
	logger := zerolog.Ctx(ctx)
	budget := s.Config.MaxEvaluations
	for _, fullName := range s.Config.Repositories {
		owner, repo, ok := strings.Cut(fullName, "/")
		if !ok {
			logger.Error().Msgf("Sweep repository %q is not owner/name", fullName)
			continue
		}
		n, err := s.sweepRepository(ctx, owner, repo, budget)
		if err != nil {
			logger.Warn().Err(err).Msgf("Failed to sweep %s", fullName)
		}
		budget -= n
		if budget <= 0 {
			return
		}
	}
}

func (s *Sweeper) sweepRepository(ctx context.Context, owner, repo string, budget int) (int, error) {
	installation, err := s.Installations.GetByRepository(ctx, owner, repo)
	if err != nil {
		return 0, errors.Wrap(err, "failed to get installation")
	}

	now := s.clock()
	s.pruneAttempts(now)
	seen := map[int]bool{}
	var candidates []sweepCandidate
	consider := func(prs []SweepPR) {
		for i := range prs {
			pr := &prs[i]
			if seen[pr.Number] {
				continue
			}
			seen[pr.Number] = true
			reason := stuckReason(pr, s.PullOpts.StatusContextFor(pr.BaseRefName), now, s.Config.PendingAge)
			if reason == "" || !s.attemptsLeft(pr.headSHA()) {
				continue
			}
			candidates = append(candidates, sweepCandidate{owner: owner, repo: repo, number: pr.Number, sha: pr.headSHA(), reason: reason})
		}
	}

	// Recently updated pull requests first: they are the ones someone is waiting on
	cursor := ""
	for {
		prs, next, err := s.Pager(ctx, installation.ID, owner, repo, cursor)
		if err != nil {
			return 0, errors.Wrap(err, "failed to list recent pull requests")
		}
		consider(prs)
		if next == "" || len(prs) == 0 || prs[len(prs)-1].UpdatedAt.Before(now.Add(-s.Config.Window)) {
			break
		}
		cursor = next
	}

	// Then one page further back each pass, wrapping around, so every open pull request is reached in turn
	key := owner + "/" + repo
	s.mu.Lock()
	backlogCursor := s.backlog[key]
	s.mu.Unlock()
	prs, next, err := s.Pager(ctx, installation.ID, owner, repo, backlogCursor)
	if err != nil {
		return 0, errors.Wrap(err, "failed to list older pull requests")
	}
	consider(prs)
	s.mu.Lock()
	if s.backlog == nil {
		s.backlog = map[string]string{}
	}
	s.backlog[key] = next
	s.mu.Unlock()

	// Fewest attempts first, so pull requests the sweep keeps retrying do not crowd out new ones under the cap
	s.mu.Lock()
	sort.SliceStable(candidates, func(i, j int) bool {
		return s.attempts[candidates[i].sha].count < s.attempts[candidates[j].sha].count
	})
	s.mu.Unlock()
	if len(candidates) > budget {
		candidates = candidates[:budget]
	}
	s.recordAttempts(candidates, now)
	s.evaluate(ctx, installation.ID, candidates)
	return len(candidates), nil
}

func (s *Sweeper) evaluate(ctx context.Context, installationID int64, candidates []sweepCandidate) {
	logger := zerolog.Ctx(ctx)
	slots := make(chan struct{}, sweepConcurrency)
	var wg sync.WaitGroup
	for _, c := range candidates {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			logger.Info().Int("github_pr_num", c.number).Str(LogKeyGitHubSHA, c.sha).Msgf("Sweep evaluating pull request: %s", c.reason)
			loc := pull.Locator{Owner: c.owner, Repo: c.repo, Number: c.number}
			var err error
			if s.evaluatePR != nil {
				err = s.evaluatePR(ctx, installationID, loc)
			} else {
				err = s.Evaluate(ctx, installationID, common.TriggerAll, loc)
			}
			if err != nil {
				logger.Warn().Err(err).Int("github_pr_num", c.number).Msg("Sweep failed to evaluate pull request")
			}
		})
	}
	wg.Wait()
}

// attemptsLeft reports whether the sweep has evaluated sha fewer than sweepMaxAttempts times.
func (s *Sweeper) attemptsLeft(sha string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[sha].count < sweepMaxAttempts
}

func (s *Sweeper) recordAttempts(candidates []sweepCandidate, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts == nil {
		s.attempts = map[string]sweepAttempt{}
	}
	for _, c := range candidates {
		s.attempts[c.sha] = sweepAttempt{count: s.attempts[c.sha].count + 1, at: now}
	}
}

// pruneAttempts forgets attempts from a day ago, so a pull request that was given up on is tried again eventually.
func (s *Sweeper) pruneAttempts(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sha, a := range s.attempts {
		if now.Sub(a.at) > 24*time.Hour {
			delete(s.attempts, sha)
		}
	}
}

func (s *Sweeper) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// GraphQLPullRequestPager reads open pull requests with the installation's GraphQL client. A page costs about one point of the GraphQL rate limit.
func GraphQLPullRequestPager(cc interface {
	NewInstallationV4Client(installationID int64) (*githubv4.Client, error)
}) PullRequestPager {
	return func(ctx context.Context, installationID int64, owner, repo, cursor string) ([]SweepPR, string, error) {
		client, err := cc.NewInstallationV4Client(installationID)
		if err != nil {
			return nil, "", err
		}
		var q struct {
			Repository struct {
				PullRequests struct {
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
					Nodes []SweepPR
				} `graphql:"pullRequests(states: OPEN, first: $first, after: $cursor, orderBy: {field: UPDATED_AT, direction: DESC})"`
			} `graphql:"repository(owner: $owner, name: $name)"`
		}
		vars := map[string]any{
			"owner":  githubv4.String(owner),
			"name":   githubv4.String(repo),
			"first":  githubv4.Int(sweepPageSize),
			"cursor": (*githubv4.String)(nil),
		}
		if cursor != "" {
			vars["cursor"] = githubv4.NewString(githubv4.String(cursor))
		}
		if err := client.Query(ctx, &q, vars); err != nil {
			return nil, "", errors.Wrap(err, "failed to query open pull requests")
		}
		next := ""
		if q.Repository.PullRequests.PageInfo.HasNextPage {
			next = q.Repository.PullRequests.PageInfo.EndCursor
		}
		return q.Repository.PullRequests.Nodes, next, nil
	}
}
