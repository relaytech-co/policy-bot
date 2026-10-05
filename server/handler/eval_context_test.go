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
	"testing"

	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/pull"
	"github.com/palantir/policy-bot/pull/pulltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseConfigSuppressesStatusForUnseenPolicy(t *testing.T) {
	ec := makeEvalContext(false)

	evaluator, err := ec.ParseConfig(context.Background(), common.TriggerAll)
	require.Error(t, err)
	assert.Nil(t, evaluator)
	assert.Nil(t, ec.Status)
}

func TestParseConfigPostsStatusForSeenPolicy(t *testing.T) {
	ec := makeEvalContext(true)

	evaluator, err := ec.ParseConfig(context.Background(), common.TriggerAll)
	require.Error(t, err)
	assert.Nil(t, evaluator)
	require.NotNil(t, ec.Status)
	assert.Equal(t, "error", ec.Status.GetState())
	assert.Equal(t, "policy-bot: main", ec.Status.GetContext())
	assert.Equal(t, "Error loading policy from testorg/testrepo@main", ec.Status.GetDescription())
}

func makeEvalContext(seenPolicy bool) *EvalContext {
	return &EvalContext{
		Options: &PullEvaluationOptions{
			StatusCheckContext: "policy-bot",
		},
		PublicURL: "https://policy-bot.example.com",
		PullContext: &pulltest.Context{
			OwnerValue:     "testorg",
			RepoValue:      "testrepo",
			NumberValue:    42,
			StateValue:     "open",
			HeadSHAValue:   "abc123",
			BranchBaseName: "main",
			BranchHeadName: "feature",
		},
		SkipPostStatus: true,
		Config: FetchedConfig{
			LoadError:  errors.New("request failed"),
			Source:     "testorg/testrepo@main",
			Path:       ".policy.yml",
			SeenPolicy: seenPolicy,
		},
	}
}

type staticEvaluator common.Result

func (e *staticEvaluator) Trigger() common.Trigger { return common.TriggerAll }

func (e *staticEvaluator) Evaluate(ctx context.Context, prctx pull.Context) common.Result {
	return common.Result(*e)
}

func TestEvaluatePolicyStatusState(t *testing.T) {
	tests := map[string]struct {
		Result   common.Result
		Expected string
	}{
		"approved":          {common.Result{Status: common.StatusApproved}, "success"},
		"disapproved":       {common.Result{Status: common.StatusDisapproved}, "failure"},
		"pendingAndWaiting": {common.Result{Status: common.StatusPending}, "pending"},
		"pendingAndFailed":  {common.Result{Status: common.StatusPending, Failed: true}, "failure"},
		"skipped":           {common.Result{Status: common.StatusSkipped}, "error"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ec := makeEvalContext(true)
			evaluator := staticEvaluator(test.Result)

			_, err := ec.EvaluatePolicy(context.Background(), &evaluator)
			require.NoError(t, err)
			require.NotNil(t, ec.Status)
			assert.Equal(t, test.Expected, ec.Status.GetState())
		})
	}
}
