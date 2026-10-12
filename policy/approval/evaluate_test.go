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

package approval

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/policy/predicate"
	"github.com/palantir/policy-bot/pull"
	"github.com/palantir/policy-bot/pull/pulltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestRules(t *testing.T) {
	ruleText := `
- name: rule1
  if:
    changed_files:
      paths: ["path1"]
    no_changed_files:
      paths: ["path2"]
    only_changed_files:
      paths: ["path3"]
    has_author_in:
      teams: ["team1"]
      users: ["user1", "user2"]
      organizations: ["org1"]
    has_contributor_in:
      teams: ["team2"]
      users: ["user3"]
      organizations: ["org2"]
  options:
    allow_author: true
    allow_contributor: true
    # invalidate_on_push: true
    methods:
      comments: ["+1"]
      github_review: false
  requires:
    users: ["user4"]
    teams: ["team3", "team4"]
    organizations: ["org3"]
    count: 5
`

	var rules []*Rule
	require.NoError(t, yaml.UnmarshalStrict([]byte(ruleText), &rules))

	expected := []*Rule{
		{
			Name: "rule1",
			Predicates: predicate.Predicates{
				ChangedFiles: &predicate.ChangedFiles{
					Paths: []common.Regexp{
						common.NewCompiledRegexp(regexp.MustCompile("path1")),
					},
				},
				NoChangedFiles: &predicate.NoChangedFiles{
					Paths: []common.Regexp{
						common.NewCompiledRegexp(regexp.MustCompile("path2")),
					},
				},
				OnlyChangedFiles: &predicate.OnlyChangedFiles{
					Paths: []common.Regexp{
						common.NewCompiledRegexp(regexp.MustCompile("path3")),
					},
				},
				HasAuthorIn: &predicate.HasAuthorIn{
					Teams:         []string{"team1"},
					Users:         []string{"user1", "user2"},
					Organizations: []string{"org1"},
				},
				HasContributorIn: &predicate.HasContributorIn{
					Teams:         []string{"team2"},
					Users:         []string{"user3"},
					Organizations: []string{"org2"},
				},
			},
			Options: Options{
				AllowAuthor:      new(true),
				AllowContributor: new(true),
				Methods: &common.Methods{
					Comments:     []string{"+1"},
					GithubReview: new(false),
				},
			},
			Requires: Requires{
				Count: 5,
				Actors: common.Actors{
					Users:         []string{"user4"},
					Teams:         []string{"team3", "team4"},
					Organizations: []string{"org3"},
				},
			},
		},
	}

	require.True(t, reflect.DeepEqual(expected, rules))
}

type mockRequirement struct {
	result *common.Result
}

func (m *mockRequirement) Trigger() common.Trigger {
	return common.TriggerStatic
}

func (m *mockRequirement) Evaluate(ctx context.Context, prctx pull.Context) common.Result {
	return *m.result
}

func makeRulesResultingIn(es ...common.EvaluationStatus) []common.Evaluator {
	var requirements []common.Evaluator
	for _, e := range es {
		requirements = append(requirements, &mockRequirement{
			result: &common.Result{
				Status: e,
			},
		})
	}
	return requirements
}

func TestAndRequirement(t *testing.T) {
	ctx := context.Background()
	prctx := &pulltest.Context{}

	// One pending is pending
	and := &AndRequirement{
		requirements: makeRulesResultingIn(common.StatusApproved, common.StatusPending),
	}
	result := and.Evaluate(ctx, prctx)
	assert.NoError(t, result.Error)
	assert.Equal(t, common.StatusPending, result.Status)

	// All approved is approved
	and = &AndRequirement{
		requirements: makeRulesResultingIn(common.StatusApproved),
	}
	result = and.Evaluate(ctx, prctx)
	assert.NoError(t, result.Error)
	assert.Equal(t, common.StatusApproved, result.Status)

	// Skipped counts
	and = &AndRequirement{
		requirements: makeRulesResultingIn(common.StatusApproved, common.StatusSkipped),
	}
	result = and.Evaluate(ctx, prctx)
	assert.NoError(t, result.Error)
	assert.Equal(t, common.StatusApproved, result.Status)

	// Skipped itself is results in Skipped
	and = &AndRequirement{
		requirements: makeRulesResultingIn(common.StatusSkipped),
	}
	result = and.Evaluate(ctx, prctx)
	assert.NoError(t, result.Error)
	assert.Equal(t, common.StatusSkipped, result.Status)

	// Error blocks approval
	and = &AndRequirement{
		requirements: []common.Evaluator{
			&mockRequirement{
				result: &common.Result{
					Status: common.StatusApproved,
				},
			},
			&mockRequirement{
				result: &common.Result{
					Error: errors.New("error"),
				},
			},
		},
	}
	result = and.Evaluate(ctx, prctx)
	assert.Error(t, result.Error)
}

func TestOrRequirement(t *testing.T) {
	ctx := context.Background()
	prctx := &pulltest.Context{}

	// One approval is enough
	or := &OrRequirement{
		requirements: makeRulesResultingIn(common.StatusPending, common.StatusSkipped, common.StatusApproved),
	}
	result := or.Evaluate(ctx, prctx)
	assert.NoError(t, result.Error)
	assert.Equal(t, common.StatusApproved, result.Status)

	// All approved is approved
	or = &OrRequirement{
		requirements: makeRulesResultingIn(common.StatusApproved),
	}
	result = or.Evaluate(ctx, prctx)
	assert.NoError(t, result.Error)
	assert.Equal(t, common.StatusApproved, result.Status)

	// Skipped does not affect approval
	or = &OrRequirement{
		requirements: makeRulesResultingIn(common.StatusApproved, common.StatusSkipped),
	}
	result = or.Evaluate(ctx, prctx)
	assert.NoError(t, result.Error)
	assert.Equal(t, common.StatusApproved, result.Status)

	// Skipped itself results in Skipped
	or = &OrRequirement{
		requirements: makeRulesResultingIn(common.StatusSkipped),
	}
	result = or.Evaluate(ctx, prctx)
	assert.NoError(t, result.Error)
	assert.Equal(t, common.StatusSkipped, result.Status)

	// Error does not block approval
	or = &OrRequirement{
		requirements: []common.Evaluator{
			&mockRequirement{
				result: &common.Result{
					Status: common.StatusApproved,
				},
			},
			&mockRequirement{
				result: &common.Result{
					Error: errors.New("error"),
				},
			},
		},
	}
	result = or.Evaluate(ctx, prctx)
	assert.NoError(t, result.Error)
	assert.Equal(t, common.StatusApproved, result.Status)
}

func pendingRequirement(failed bool) common.Evaluator {
	return &mockRequirement{
		result: &common.Result{
			Status: common.StatusPending,
			Failed: failed,
		},
	}
}

func TestRequirementFailedPropagation(t *testing.T) {
	ctx := context.Background()
	prctx := &pulltest.Context{}

	t.Run("andFailsOnAnyFailedRule", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			pendingRequirement(false),
			pendingRequirement(true),
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, common.StatusPending, result.Status)
		assert.True(t, result.Failed)
	})

	t.Run("andDoesNotFailWhenOnlyWaiting", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			pendingRequirement(false),
			pendingRequirement(false),
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, common.StatusPending, result.Status)
		assert.False(t, result.Failed)
	})

	t.Run("orDoesNotFailWhileOneRuleCouldStillApprove", func(t *testing.T) {
		or := &OrRequirement{requirements: []common.Evaluator{
			pendingRequirement(true),
			pendingRequirement(false),
		}}
		result := or.Evaluate(ctx, prctx)
		assert.Equal(t, common.StatusPending, result.Status)
		assert.False(t, result.Failed)
	})

	t.Run("orFailsWhenEveryRuleFailed", func(t *testing.T) {
		or := &OrRequirement{requirements: []common.Evaluator{
			pendingRequirement(true),
			pendingRequirement(true),
		}}
		result := or.Evaluate(ctx, prctx)
		assert.Equal(t, common.StatusPending, result.Status)
		assert.True(t, result.Failed)
	})

	t.Run("approvedIsNeverFailed", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			&mockRequirement{result: &common.Result{Status: common.StatusApproved}},
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, common.StatusApproved, result.Status)
		assert.False(t, result.Failed)
	})
}

func namedRule(name string, status common.EvaluationStatus, failed bool) common.Evaluator {
	return &mockRequirement{
		result: &common.Result{
			Name:   name,
			Status: status,
			Failed: failed,
		},
	}
}

func TestPendingStatusDescription(t *testing.T) {
	ctx := context.Background()
	prctx := &pulltest.Context{}

	t.Run("andNamesFailedBeforeWaiting", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			namedRule("always", common.StatusApproved, false),
			namedRule("relay_api lint and bazel-checks", common.StatusPending, false),
			namedRule("lint-terraform", common.StatusSkipped, false),
			namedRule("check-codegen", common.StatusPending, true),
			namedRule("dbt CI", common.StatusPending, false),
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, "Failed: check-codegen · Waiting: relay_api lint and bazel-checks, dbt CI", result.StatusDescription)
	})

	t.Run("onlyWaiting", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			namedRule("always", common.StatusApproved, false),
			namedRule("nelson ci", common.StatusPending, false),
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, "Waiting: nelson ci", result.StatusDescription)
	})

	t.Run("onlyFailed", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			namedRule("Lint Rust Code", common.StatusPending, true),
			namedRule("dagster CI", common.StatusPending, true),
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, "Failed: Lint Rust Code, dagster CI", result.StatusDescription)
	})

	t.Run("nestedAndNamesInnerRules", func(t *testing.T) {
		// The shape relaycode's generated .policy.yml has: one "and" holding
		// every rule.
		inner := &AndRequirement{requirements: []common.Evaluator{
			namedRule("always", common.StatusApproved, false),
			namedRule("relay_api lint and bazel-checks", common.StatusPending, false),
			namedRule("check-codegen", common.StatusPending, true),
		}}
		approval := &evaluator{root: &AndRequirement{requirements: []common.Evaluator{inner}}}

		result := approval.Evaluate(ctx, prctx)
		assert.Equal(t, common.StatusPending, result.Status)
		assert.True(t, result.Failed)
		assert.Equal(t, "Failed: check-codegen · Waiting: relay_api lint and bazel-checks", result.StatusDescription)
	})

	t.Run("nestedOrAndRepeatedRules", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			namedRule("dbt CI", common.StatusPending, false),
			&OrRequirement{requirements: []common.Evaluator{
				namedRule("dbt CI", common.StatusPending, false),
				namedRule("cube-lint-yaml and cube-validate", common.StatusPending, true),
			}},
			// An approved "or" is satisfied, so its pending rules are not named.
			&OrRequirement{requirements: []common.Evaluator{
				namedRule("always", common.StatusApproved, false),
				namedRule("RCP Build & Test", common.StatusPending, false),
			}},
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, "Failed: cube-lint-yaml and cube-validate · Waiting: dbt CI", result.StatusDescription)
	})

	t.Run("orNamesRules", func(t *testing.T) {
		or := &OrRequirement{requirements: []common.Evaluator{
			namedRule("check-codegen", common.StatusPending, true),
			namedRule("nelson ci", common.StatusPending, false),
			namedRule("lint-terraform", common.StatusSkipped, false),
		}}
		result := or.Evaluate(ctx, prctx)
		assert.Equal(t, "Failed: check-codegen · Waiting: nelson ci", result.StatusDescription)
	})

	t.Run("truncatesAt140Characters", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			namedRule("relay_api lint and bazel-checks", common.StatusPending, false),
			namedRule("Shipments Core Build & Test", common.StatusPending, false),
			namedRule("cube-lint-yaml and cube-validate", common.StatusPending, false),
			namedRule("policy-bot config is generated", common.StatusPending, false),
			namedRule("check-codegen", common.StatusPending, true),
			namedRule("RCP Build & Test", common.StatusPending, true),
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, "Failed: check-codegen, RCP Build & Test · Waiting: relay_api lint and bazel-checks, Shipments Core Build & Test +2 more", result.StatusDescription)
		assert.LessOrEqual(t, utf8.RuneCountInString(result.StatusDescription), 140)
	})

	t.Run("fitsExactly140Characters", func(t *testing.T) {
		// "Waiting: " is 9 characters, so a 131 character name fills the
		// description without needing "+N more".
		name := strings.Repeat("x", 131)
		and := &AndRequirement{requirements: []common.Evaluator{
			namedRule(name, common.StatusPending, false),
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, "Waiting: "+name, result.StatusDescription)
	})

	t.Run("fallsBackToCountWhenNoNameFits", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			namedRule("always", common.StatusApproved, false),
			namedRule(strings.Repeat("x", 132), common.StatusPending, false),
		}}
		result := and.Evaluate(ctx, prctx)
		assert.Equal(t, "1/2 rules approved", result.StatusDescription)
	})

	t.Run("approvedAndSkippedDescriptionsUnchanged", func(t *testing.T) {
		and := &AndRequirement{requirements: []common.Evaluator{
			namedRule("always", common.StatusApproved, false),
		}}
		assert.Equal(t, "All rules are approved", and.Evaluate(ctx, prctx).StatusDescription)

		and = &AndRequirement{requirements: []common.Evaluator{
			namedRule("lint-terraform", common.StatusSkipped, false),
		}}
		assert.Equal(t, "All of the rules are skipped", and.Evaluate(ctx, prctx).StatusDescription)
	})
}
