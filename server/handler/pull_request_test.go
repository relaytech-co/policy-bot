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
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/palantir/policy-bot/policy/common"
	"github.com/stretchr/testify/assert"
)

func TestPullRequestTrigger(t *testing.T) {
	commitAndStatus := common.TriggerCommit | common.TriggerStatus

	tests := map[string]struct {
		Event    github.PullRequestEvent
		Expected common.Trigger
		// Whether a policy that only depends on commits and statuses is evaluated
		Evaluates bool
	}{
		"opened": {
			Event:     github.PullRequestEvent{Action: new("opened")},
			Expected:  common.TriggerCommit | common.TriggerPullRequest,
			Evaluates: true,
		},
		"titleEdited": {
			Event:    github.PullRequestEvent{Action: new("edited"), Changes: &github.EditChange{Title: &github.EditTitle{From: new("old")}}},
			Expected: common.TriggerPullRequest,
		},
		"baseChanged": {
			Event:     github.PullRequestEvent{Action: new("edited"), Changes: &github.EditChange{Base: &github.EditBase{Ref: &github.EditRef{From: new("gtmq_spec")}}}},
			Expected:  common.TriggerAll,
			Evaluates: true,
		},
		"closed": {
			Event:    github.PullRequestEvent{Action: new("closed")},
			Expected: common.TriggerStatic,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			trigger := pullRequestTrigger(test.Event)
			assert.Equal(t, test.Expected, trigger)
			assert.Equal(t, test.Evaluates, trigger.Matches(commitAndStatus))
		})
	}
}
