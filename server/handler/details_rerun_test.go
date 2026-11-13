package handler

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/palantir/policy-bot/policy"
	"github.com/palantir/policy-bot/policy/approval"
	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/policy/predicate"
	"github.com/palantir/policy-bot/pull"
	"github.com/palantir/policy-bot/pull/pulltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func workflowConfig(paths ...string) *policy.Config {
	return &policy.Config{
		ApprovalRules: []*approval.Rule{{
			Name:       "CI must pass",
			Predicates: predicate.Predicates{HasWorkflowResult: predicate.NewHasWorkflowResult(paths, nil)},
		}},
	}
}

func TestWorkflowRunRows(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	run := func(id int64, path, status, conclusion string, minutesAgo int) pull.WorkflowRun {
		return pull.WorkflowRun{ID: id, Path: path, Event: "pull_request", Status: status, Conclusion: conclusion, StartedAt: now.Add(-time.Duration(minutesAgo) * time.Minute)}
	}

	ec := &EvalContext{
		Config: FetchedConfig{Config: workflowConfig(".github/workflows/a.yml", ".github/workflows/b.yml", ".github/workflows/c.yml", ".github/workflows/d.yml")},
		PullContext: &pulltest.Context{NewestWorkflowRunsValue: []pull.WorkflowRun{
			run(1, ".github/workflows/a.yml", "completed", "success", 18),
			run(2, ".github/workflows/b.yml", "completed", "timed_out", 9),
			run(3, ".github/workflows/c.yml", "in_progress", "", 1),
			run(4, ".github/workflows/d.yml", "completed", "skipped", 30),
			run(5, ".github/workflows/unread.yml", "completed", "failure", 5),
		}},
	}

	rows, err := workflowRunRows(ec, now)
	require.NoError(t, err)

	require.Len(t, rows, 4, "a workflow the policy does not read is not listed")
	assert.Equal(t, WorkflowRunRow{ID: 2, Name: "b.yml", Event: "pull_request", Conclusion: "timed out", Ago: "9 min ago", Rerunnable: true}, rows[0], "failed runs come first")
	conclusions := make(map[string]string)
	for _, row := range rows[1:] {
		assert.False(t, row.Rerunnable, "%s is not re-runnable", row.Name)
		conclusions[row.Name] = row.Conclusion
	}
	assert.Equal(t, map[string]string{"a.yml": "success", "c.yml": "in progress", "d.yml": "skipped"}, conclusions)
	assert.True(t, anyRerunnable(rows))
}

func TestWorkflowRunRowsWithoutPolicy(t *testing.T) {
	ec := &EvalContext{PullContext: &pulltest.Context{NewestWorkflowRunsValue: []pull.WorkflowRun{{ID: 1, Path: "a.yml"}}}}
	rows, err := workflowRunRows(ec, time.Now())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestAgo(t *testing.T) {
	assert.Equal(t, "just now", ago(20*time.Second))
	assert.Equal(t, "14 min ago", ago(14*time.Minute+30*time.Second))
	assert.Equal(t, "3 h ago", ago(3*time.Hour))
	assert.Equal(t, "3 days ago", ago(72*time.Hour))
}

func TestCanRerun(t *testing.T) {
	prctx := &pulltest.Context{
		CollaboratorsValue: []*pull.Collaborator{
			{Name: "writer", Permissions: []pull.CollaboratorPermission{{Permission: pull.PermissionWrite}}},
			{Name: "reader", Permissions: []pull.CollaboratorPermission{{Permission: pull.PermissionRead}}},
		},
	}
	ctx := context.Background()

	t.Run("defaultsToWriteAccess", func(t *testing.T) {
		opts := &PullEvaluationOptions{}
		ok, err := opts.CanRerun(ctx, prctx, "writer")
		require.NoError(t, err)
		assert.True(t, ok)

		ok, err = opts.CanRerun(ctx, prctx, "reader")
		require.NoError(t, err)
		assert.False(t, ok)

		ok, err = opts.CanRerun(ctx, prctx, "stranger")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("configured", func(t *testing.T) {
		opts := &PullEvaluationOptions{Rerunners: &common.Actors{Users: []string{"reader"}}}
		ok, err := opts.CanRerun(ctx, prctx, "reader")
		require.NoError(t, err)
		assert.True(t, ok)

		ok, err = opts.CanRerun(ctx, prctx, "writer")
		require.NoError(t, err)
		assert.False(t, ok, "a configured list replaces the default")
	})
}

func TestPullRequestURL(t *testing.T) {
	pr := &github.PullRequest{
		Number:  github.Ptr(68231),
		HTMLURL: github.Ptr("https://github.com/relaytech-co/relaycode/pull/68231"),
		Base:    &github.PullRequestBranch{Repo: &github.Repository{Name: github.Ptr("relaycode"), Owner: &github.User{Login: github.Ptr("relaytech-co")}}},
	}

	assert.Equal(t, "https://github.com/relaytech-co/relaycode/pull/68231", (&PullEvaluationOptions{}).PullRequestURL(pr))

	opts := &PullEvaluationOptions{PullRequestURLTemplate: "https://app.graphite.com/github/pr/{owner}/{repo}/{number}"}
	assert.Equal(t, "https://app.graphite.com/github/pr/relaytech-co/relaycode/68231", opts.PullRequestURL(pr))
}
