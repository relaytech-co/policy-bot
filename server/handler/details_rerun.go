package handler

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/palantir/policy-bot/pull"
	"github.com/pkg/errors"
)

// rerunnableConclusions are the conclusions of a finished workflow run that re-running can change.
var rerunnableConclusions = []string{"failure", "timed_out", "cancelled"}

// WorkflowRunRow is a workflow the policy reads, as the details page lists it.
type WorkflowRunRow struct {
	ID         int64
	Name       string
	Event      string
	Conclusion string
	Ago        string
	URL        string
	Rerunnable bool
}

// workflowRunRows lists the newest run of each workflow the policy reads, failed ones first.
func workflowRunRows(ec *EvalContext, now time.Time) ([]WorkflowRunRow, error) {
	if ec.Config.Config == nil {
		return nil, nil
	}
	observed := ec.Config.Config.ReferencedWorkflows()
	if len(observed) == 0 {
		return nil, nil
	}

	runs, err := ec.PullContext.NewestWorkflowRuns()
	if err != nil {
		return nil, err
	}

	var rows []WorkflowRunRow
	for _, run := range runs {
		if _, ok := observed[run.Path]; !ok {
			continue
		}
		rows = append(rows, newWorkflowRunRow(run, now))
	}
	slices.SortStableFunc(rows, func(a, b WorkflowRunRow) int {
		switch {
		case a.Rerunnable == b.Rerunnable:
			return 0
		case a.Rerunnable:
			return -1
		default:
			return 1
		}
	})
	return rows, nil
}

func newWorkflowRunRow(run pull.WorkflowRun, now time.Time) WorkflowRunRow {
	conclusion := run.Conclusion
	if run.Status != "completed" {
		conclusion = run.Status
	}
	return WorkflowRunRow{
		ID:         run.ID,
		Name:       path.Base(run.Path),
		Event:      run.Event,
		Conclusion: strings.ReplaceAll(conclusion, "_", " "),
		Ago:        ago(now.Sub(run.StartedAt)),
		URL:        run.URL,
		Rerunnable: run.Status == "completed" && slices.Contains(rerunnableConclusions, run.Conclusion),
	}
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func anyRerunnable(rows []WorkflowRunRow) bool {
	return slices.ContainsFunc(rows, func(r WorkflowRunRow) bool { return r.Rerunnable })
}

// canRerun reports whether the user may re-run workflows of the pull request, and whether there is anything to re-run.
func (h *Details) canRerun(ctx context.Context, state *DetailsState, rows []WorkflowRunRow) (bool, error) {
	if state.PullRequest.GetState() != "open" || !anyRerunnable(rows) {
		return false, nil
	}
	return h.PullOpts.CanRerun(ctx, state.EvalContext.PullContext, state.Username)
}

// DetailsRerun re-runs the failed jobs of the workflows a user picked on the details page.
type DetailsRerun struct {
	Details
}

func (h *DetailsRerun) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	state := h.getStateIfAllowed(w, r)
	if state == nil {
		return nil
	}

	canRerun, err := h.PullOpts.CanRerun(state.Ctx, state.EvalContext.PullContext, state.Username)
	if err != nil {
		return errors.Wrap(err, "failed to check whether the user may re-run workflows")
	}
	if !canRerun {
		http.Error(w, "You are not allowed to re-run workflows", http.StatusForbidden)
		return nil
	}
	if state.PullRequest.GetState() != "open" {
		http.Error(w, "The pull request is not open", http.StatusBadRequest)
		return nil
	}
	// The form carries the commit it was rendered for, so runs of a newer commit are not re-run unseen
	if sha := r.FormValue("sha"); sha != state.PullRequest.GetHead().GetSHA() {
		http.Error(w, "The pull request has changed since the page was loaded, reload it and try again", http.StatusConflict)
		return nil
	}

	rows, err := workflowRunRows(state.EvalContext, time.Now())
	if err != nil {
		return errors.Wrap(err, "failed to list workflow runs")
	}

	// Only runs the page would offer are re-run, whatever ids were posted
	picked := make(map[int64]bool)
	for _, v := range r.Form["run"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			picked[id] = true
		}
	}
	var runs []WorkflowRunRow
	for _, row := range rows {
		if row.Rerunnable && picked[row.ID] {
			runs = append(runs, row)
		}
	}
	if len(runs) == 0 {
		http.Error(w, "None of the selected workflows can be re-run, reload the page and try again", http.StatusConflict)
		return nil
	}

	owner, repo, number, _ := parsePullParams(r)
	for _, run := range runs {
		state.Logger.Info().
			Str(LogKeyAudit, "rerun").
			Str(LogKeyGitHubSHA, state.PullRequest.GetHead().GetSHA()).
			Int64("workflow_run_id", run.ID).
			Msgf("User '%s' re-ran the failed jobs of %s", state.Username, run.Name)

		if _, err := state.EvalContext.Client.Actions.RerunFailedJobsByID(state.Ctx, owner, repo, run.ID); err != nil {
			return errors.Wrapf(err, "failed to re-run workflow run %d", run.ID)
		}
	}

	http.Redirect(w, r, fmt.Sprintf("%s/details/%s/%s/%d", getBasePath(h.BaseConfig.PublicURL), owner, repo, number), http.StatusSeeOther)
	return nil
}
