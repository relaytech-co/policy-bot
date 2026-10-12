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
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/palantir/policy-bot/policy/common"
	"github.com/palantir/policy-bot/pull"
	"github.com/rs/zerolog"
)

type evaluator struct {
	root common.Evaluator
}

func (eval *evaluator) Trigger() common.Trigger {
	if eval.root != nil {
		return eval.root.Trigger()
	}
	return common.TriggerStatic
}

func (eval *evaluator) Evaluate(ctx context.Context, prctx pull.Context) (res common.Result) {
	if eval.root != nil {
		res = eval.root.Evaluate(ctx, prctx)
	} else {
		zerolog.Ctx(ctx).Debug().Msg("No approval policy defined; skipping")

		res.Status = common.StatusApproved
		res.StatusDescription = "No approval policy defined"
	}

	res.Name = "approval"
	return
}

type RuleRequirement struct {
	rule *Rule
}

func (r *RuleRequirement) Trigger() common.Trigger {
	return r.rule.Trigger()
}

func (r *RuleRequirement) Evaluate(ctx context.Context, prctx pull.Context) common.Result {
	log := zerolog.Ctx(ctx).With().Str("rule", r.rule.Name).Logger()
	ctx = log.WithContext(ctx)

	result := r.rule.Evaluate(ctx, prctx)
	if result.Error == nil {
		log.Debug().Msgf("rule evaluation resulted in %s:\"%s\"", result.Status, result.StatusDescription)
	} else {
		// Log rule evaluations that fail at info level so they appear in logs
		// by default, but don't log them as warnings or errors since they
		// don't necessarily break the overall policy (e.g. an 'or' rule can
		// suppress an error if other members are pending or approved.) Having
		// this information in logs is useful to understand the rate of
		// particular types of failures across a policy-bot installation.
		log.Info().Err(result.Error).Msg("rule evaluation resulted in error")
	}

	return result
}

type OrRequirement struct {
	requirements []common.Evaluator
}

func (r *OrRequirement) Trigger() common.Trigger {
	var t common.Trigger
	for _, child := range r.requirements {
		t |= child.Trigger()
	}
	return t
}

func (r *OrRequirement) Evaluate(ctx context.Context, prctx pull.Context) common.Result {
	var children []*common.Result
	for _, req := range r.requirements {
		res := req.Evaluate(ctx, prctx)
		children = append(children, &res)
	}

	var err error
	var pending, approved, skipped, failed int
	for _, c := range children {
		if c.Error != nil {
			err = c.Error
			continue
		}

		switch c.Status {
		case common.StatusApproved:
			approved++
		case common.StatusPending:
			pending++
			if c.Failed {
				failed++
			}
		case common.StatusSkipped:
			skipped++
		}
	}

	var status common.EvaluationStatus
	description := "All of the rules are skipped"

	switch {
	case approved > 0:
		status = common.StatusApproved
		description = "One or more rules approved"
		err = nil
	case pending > 0:
		status = common.StatusPending
		description = pendingDescription(children, "None of the rules are satisfied")
		err = nil
	}

	return common.Result{
		Name: "or",
		// Any rule that is merely waiting could still approve, so only a branch
		// where every pending rule has failed is itself failed.
		Failed:            status == common.StatusPending && failed == pending,
		Status:            status,
		StatusDescription: description,
		Error:             err,
		Children:          children,
	}
}

type AndRequirement struct {
	requirements []common.Evaluator
}

func (r *AndRequirement) Trigger() common.Trigger {
	var t common.Trigger
	for _, child := range r.requirements {
		t |= child.Trigger()
	}
	return t
}

func (r *AndRequirement) Evaluate(ctx context.Context, prctx pull.Context) common.Result {
	var children []*common.Result
	for _, req := range r.requirements {
		res := req.Evaluate(ctx, prctx)
		children = append(children, &res)
	}

	var err error
	var pending, approved, skipped, failed int
	for _, c := range children {
		if c.Error != nil {
			err = c.Error
			continue
		}

		switch c.Status {
		case common.StatusApproved:
			approved++
		case common.StatusPending:
			pending++
			if c.Failed {
				failed++
			}
		case common.StatusSkipped:
			skipped++
		}
	}

	var status common.EvaluationStatus
	description := "All of the rules are skipped"

	switch {
	case approved > 0 && pending == 0:
		status = common.StatusApproved
		description = fmt.Sprintf("All rules are approved")
	case pending > 0:
		status = common.StatusPending
		description = pendingDescription(children, fmt.Sprintf("%d/%d rules approved", approved, approved+pending))
	}

	return common.Result{
		Name: "and",
		// Every rule has to approve, so one failure is enough to fail the branch.
		Failed:            status == common.StatusPending && failed > 0,
		Status:            status,
		StatusDescription: description,
		Error:             err,
		Children:          children,
	}
}

// maxStatusDescription is the longest description GitHub accepts on a commit
// status.
const maxStatusDescription = 140

// pendingDescription names the rules under children that are still pending,
// failed ones first, for example "Failed: check-codegen · Waiting: dbt CI".
// Nested and/or results are walked down to their rules, so that a policy
// with a single top-level "and" still names its rules. Rules that do not fit
// in a commit status description are counted as "+N more". It returns
// fallback when no rule name fits.
func pendingDescription(children []*common.Result, fallback string) string {
	var failed, waiting []string
	collectPendingRules(children, make(map[string]bool), &failed, &waiting)

	if desc := formatPendingRules(failed, waiting, maxStatusDescription); desc != "" {
		return desc
	}
	return fallback
}

// collectPendingRules appends the name of each pending rule under results to
// failed or waiting, in policy order. A rule referenced from more than one
// place in the policy is listed once.
func collectPendingRules(results []*common.Result, seen map[string]bool, failed, waiting *[]string) {
	for _, r := range results {
		if r.Error != nil || r.Status != common.StatusPending {
			continue
		}
		if len(r.Children) > 0 {
			collectPendingRules(r.Children, seen, failed, waiting)
			continue
		}
		if r.Name == "" || seen[r.Name] {
			continue
		}
		seen[r.Name] = true

		if r.Failed {
			*failed = append(*failed, r.Name)
		} else {
			*waiting = append(*waiting, r.Name)
		}
	}
}

// formatPendingRules lists as many rules as fit in limit characters, dropping
// waiting rules before failed ones, and returns "" if not even one fits.
func formatPendingRules(failed, waiting []string, limit int) string {
	total := len(failed) + len(waiting)
	for shown := total; shown > 0; shown-- {
		f := failed[:min(shown, len(failed))]
		w := waiting[:shown-len(f)]

		var sections []string
		if len(f) > 0 {
			sections = append(sections, "Failed: "+strings.Join(f, ", "))
		}
		if len(w) > 0 {
			sections = append(sections, "Waiting: "+strings.Join(w, ", "))
		}

		desc := strings.Join(sections, " · ")
		if shown < total {
			desc += fmt.Sprintf(" +%d more", total-shown)
		}
		if utf8.RuneCountInString(desc) <= limit {
			return desc
		}
	}
	return ""
}
