// Copyright 2018 Palantir Technologies, Inc.
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
	"encoding/json"

	"github.com/google/go-github/v92/github"
	"github.com/palantir/go-githubapp/githubapp"
	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/pull"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

type PullRequest struct {
	Base
}

func (h *PullRequest) Handles() []string { return []string{"pull_request"} }

// Handle pull_request
// https://developer.github.com/v3/activity/events/types/#requestevent
func (h *PullRequest) Handle(ctx context.Context, eventType, deliveryID string, payload []byte) error {
	var event github.PullRequestEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return errors.Wrap(err, "failed to parse pull request event payload")
	}

	installationID := githubapp.GetInstallationIDFromEvent(&event)
	ctx, _ = h.PreparePRContext(ctx, installationID, event.GetPullRequest())

	var t common.Trigger
	switch event.GetAction() {
	case "opened", "reopened", "ready_for_review":
		t = common.TriggerCommit | common.TriggerPullRequest
	case "synchronize":
		t = common.TriggerCommit
	case "edited":
		t = common.TriggerPullRequest
		if event.GetChanges().GetBase() != nil {
			h.invalidateStatus(ctx, installationID, event.GetPullRequest())
		}
	case "labeled", "unlabeled":
		t = common.TriggerLabel
	default:
		return nil
	}

	return h.Evaluate(ctx, installationID, t, pull.Locator{
		Owner:  event.GetRepo().GetOwner().GetLogin(),
		Repo:   event.GetRepo().GetName(),
		Number: event.GetPullRequest().GetNumber(),
		Value:  event.GetPullRequest(),
	})
}

// invalidateStatus marks the pull request pending against its new base branch, before the
// re-evaluation that the retarget triggers. Without it a status approved against the old base
// stays green on the head commit for as long as that evaluation takes, which is long enough to
// merge through. It only matters when the context carries no branch, since otherwise the new
// base has a context of its own that has never been posted.
func (h *PullRequest) invalidateStatus(ctx context.Context, installationID int64, pr *github.PullRequest) {
	logger := zerolog.Ctx(ctx)

	client, err := h.NewInstallationClient(installationID)
	if err != nil {
		logger.Err(err).Msg("Failed to create client to invalidate status after base change")
		return
	}

	state := "pending"
	message := "Re-evaluating against the new base branch"
	status := github.RepoStatus{
		Context:     new(h.PullOpts.StatusContextFor(pr.GetBase().GetRef())),
		State:       &state,
		Description: &message,
	}

	owner := pr.GetBase().GetRepo().GetOwner().GetLogin()
	repo := pr.GetBase().GetRepo().GetName()
	if err := PostStatus(ctx, client, owner, repo, pr.GetHead().GetSHA(), status); err != nil {
		logger.Err(err).Msg("Failed to invalidate status after base change")
	}
}
