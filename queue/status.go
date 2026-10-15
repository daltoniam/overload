package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
)

// StatusSetter sets overload's commit status on a pull request's head.
type StatusSetter interface {
	SetStatus(ctx context.Context, installationID int64, repository, sha, state, description, targetURL string) error
}

// Commit status states.
const (
	statusPending = "pending"
	statusSuccess = "success"
	statusFailure = "failure"
	statusError   = "error"
)

// reportStatus sets the commit status for a run, best effort: a review or
// post never fails because its status could not be set. Runs of dry-run
// repositories, local reviews and runs without an App installation get no
// status.
func reportStatus(ctx context.Context, store *postgres.Store, setter StatusSetter, runID int64, state, description string) {
	target, ok := statusTarget(ctx, store, runID)
	if !ok {
		return
	}
	if setter == nil {
		client, err := appClient(ctx, store)
		if err != nil {
			return
		}
		setter = client
	}
	err := setter.SetStatus(context.WithoutCancel(ctx), target.installation, target.repository, target.sha, state, description, runURL(runID))
	switch {
	case errors.Is(err, github.ErrStatusForbidden):
		slog.Warn("commit status not set; grant the GitHub App \"Commit statuses: write\"", "run_id", runID, "repository", target.repository)
	case err != nil:
		slog.Warn("commit status not set", "run_id", runID, "repository", target.repository, "error", err)
	}
}

type statusRun struct {
	repository   string
	sha          string
	installation int64
}

func statusTarget(ctx context.Context, store *postgres.Store, runID int64) (statusRun, bool) {
	var target statusRun
	var installation *int64
	var dryRun bool
	err := store.Pool.QueryRow(ctx, `SELECT repo.full_name, r.head_sha, r.installation_id, r.dry_run OR repo.dry_run FROM runs r JOIN repositories repo ON repo.id = r.repository_id WHERE r.id = $1 AND r.kind = 'pr_review'`, runID).Scan(&target.repository, &target.sha, &installation, &dryRun)
	if err != nil || installation == nil || *installation == 0 || dryRun || target.sha == "" {
		return target, false
	}
	target.installation = *installation
	return target, true
}

// runURL links the status to the run page when OVERLOAD_BASE_URL is a
// public https address.
func runURL(runID int64) string {
	base := strings.TrimRight(os.Getenv("OVERLOAD_BASE_URL"), "/")
	if !strings.HasPrefix(base, "https://") {
		return ""
	}
	return fmt.Sprintf("%s/runs/%d", base, runID)
}

// finalStatus describes a posted review as a commit status: failure when
// the review requested changes, success otherwise. A partial review (some
// files could not be reviewed) says so.
func finalStatus(event string, findings int, partial bool) (string, string) {
	state, description := statusSuccess, fmt.Sprintf("%d issue(s) noted as comments", findings)
	switch {
	case event == overload.ReviewRequestChanges:
		state, description = statusFailure, "Changes requested: blocking issues found"
	case event == overload.ReviewApprove:
		description = "Approved"
	case findings == 0:
		description = "No issues found"
	}
	if partial {
		description = "Partial review, some files not reviewed. " + description
	}
	return state, description
}
