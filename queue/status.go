package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
)

// StatusSetter sets overload's commit status on a pull request's head.
type StatusSetter interface {
	SetStatus(ctx context.Context, installationID int64, repository, sha, state, description, targetURL string) error
}

// Commit status states set outside a review's merge gate (see
// overload.GateOutcome).
const (
	statusPending = "pending"
	statusError   = "error"
)

// reportStatus sets the commit status for a run, best effort: a review or
// post never fails because its status could not be set. Runs of dry-run
// repositories, local reviews and runs without an App installation get no
// status, nor does a run with a newer review of the same commit, so an
// older verdict finishing last cannot replace the newer one.
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
	err := setter.SetStatus(context.WithoutCancel(ctx), target.installation, target.repository, target.sha, state, statusDescription(description), runURL(runID))
	switch {
	case errors.Is(err, github.ErrStatusForbidden):
		slog.Warn("commit status not set; grant the GitHub App \"Commit statuses: write\"", "run_id", runID, "repository", target.repository)
	case err != nil:
		slog.Warn("commit status not set", "run_id", runID, "repository", target.repository, "error", err)
	}
}

// statusDescription shortens a description to the 140 characters GitHub
// accepts.
func statusDescription(description string) string {
	const limit = 140
	if runes := []rune(description); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return description
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
	var rerun bool
	err := store.Pool.QueryRow(ctx, `SELECT repo.full_name, r.head_sha, r.installation_id, r.dry_run OR repo.dry_run,
EXISTS (SELECT 1 FROM runs n WHERE n.repository_id = r.repository_id AND n.pr_number = r.pr_number AND n.head_sha = r.head_sha AND n.id > r.id AND n.kind = 'pr_review' AND NOT n.dry_run AND n.status IN ('queued', 'running', 'completed'))
FROM runs r JOIN repositories repo ON repo.id = r.repository_id WHERE r.id = $1 AND r.kind = 'pr_review'`, runID).Scan(&target.repository, &target.sha, &installation, &dryRun, &rerun)
	if err != nil || installation == nil || *installation == 0 || dryRun || rerun || target.sha == "" {
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
