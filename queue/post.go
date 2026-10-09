package queue

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
)

// PostingEnabled is the global switch for writing to GitHub. A run is posted
// only when this is on and neither its repository nor its binding is dry-run.
func PostingEnabled() bool {
	return os.Getenv("OVERLOAD_ENABLE_POSTING") == "1"
}

type ReviewPoster interface {
	FindReview(ctx context.Context, installationID int64, repository string, number int, marker string) (int64, error)
	PostReview(ctx context.Context, installationID int64, repository string, number int, sha, event, summary string, findings []overload.Finding) (int64, error)
	ChangesRequested(ctx context.Context, installationID int64, repository string, number int) ([]int64, error)
	DismissReview(ctx context.Context, installationID int64, repository string, number int, reviewID int64, message string) error
}

// dismissMessage explains why overload withdrew its own request for changes.
const dismissMessage = "Overload's latest review of this pull request found nothing blocking."

type PostWorker struct {
	river.WorkerDefaults[postgres.PostReviewArgs]
	Store  *postgres.Store
	Poster ReviewPoster
}

func (w *PostWorker) Work(ctx context.Context, job *river.Job[postgres.PostReviewArgs]) error {
	runID := job.Args.RunID
	if !PostingEnabled() {
		return w.Store.SkipPost(ctx, runID, "posting_disabled")
	}
	target, err := w.Store.LoadPostTarget(ctx, runID)
	if errors.Is(err, postgres.ErrNothingToPost) {
		return nil
	}
	if err != nil {
		return err
	}
	if target.RepositoryPaused {
		return w.Store.SkipPost(ctx, runID, "repository_paused")
	}
	if target.Superseded {
		return w.Store.SkipPost(ctx, runID, "superseded_by_newer_commit")
	}
	if target.InstallationID == 0 {
		return w.Store.SkipPost(ctx, runID, "no_github_app_installation")
	}
	poster := w.Poster
	if poster == nil {
		client, err := appClient(ctx, w.Store)
		if err != nil {
			return w.Store.SkipPost(ctx, runID, "missing_github_app")
		}
		poster = client
	}
	marker := github.ReviewMarker(runID)
	existing, err := poster.FindReview(ctx, target.InstallationID, target.Repository, target.PRNumber, marker)
	if err != nil {
		return fmt.Errorf("check existing review: %w", err)
	}
	if existing > 0 {
		return w.Store.FinishPost(ctx, target, "posted", existing)
	}
	severities := append([]string(nil), target.DuplicateSeverities...)
	for _, finding := range target.Findings {
		severities = append(severities, finding.Severity)
	}
	event := overload.ReviewEvent(target.ReviewDecision, target.BlockSeverity, target.Partial, severities)
	decides := target.ReviewDecision == overload.ReviewDecisionRequestChanges || target.ReviewDecision == overload.ReviewDecisionApprove
	var standing []int64
	if decides {
		if standing, err = poster.ChangesRequested(ctx, target.InstallationID, target.Repository, target.PRNumber); err != nil {
			return fmt.Errorf("check earlier reviews: %w", err)
		}
	}
	if len(target.Findings) == 0 && event != overload.ReviewApprove && (event != overload.ReviewRequestChanges || len(standing) > 0) {
		if err := w.dismiss(ctx, poster, target, event, standing); err != nil {
			return err
		}
		return w.Store.FinishPost(ctx, target, "nothing_new", 0)
	}
	summary := reviewSummary(event, target, len(severities)-len(target.Findings)) + "\n\n" + marker
	reviewID, err := poster.PostReview(ctx, target.InstallationID, target.Repository, target.PRNumber, target.HeadSHA, event, summary, target.Findings)
	if errors.Is(err, github.ErrReviewRejected) {
		return w.Store.RejectPost(ctx, runID, err.Error())
	}
	if err != nil {
		return fmt.Errorf("post review: %w", err)
	}
	if err := w.dismiss(ctx, poster, target, event, standing); err != nil {
		return err
	}
	return w.Store.FinishPost(ctx, target, "posted", reviewID)
}

// dismiss withdraws overload's earlier requests for changes once a review
// no longer blocks. An approval would supersede them anyway; dismissing
// also covers request-changes-only workflows, where nothing else would.
func (w *PostWorker) dismiss(ctx context.Context, poster ReviewPoster, target postgres.PostTarget, event string, standing []int64) error {
	if event == overload.ReviewRequestChanges {
		return nil
	}
	for _, id := range standing {
		if err := poster.DismissReview(ctx, target.InstallationID, target.Repository, target.PRNumber, id, dismissMessage); err != nil {
			return fmt.Errorf("dismiss earlier review: %w", err)
		}
	}
	return nil
}

func reviewSummary(event string, target postgres.PostTarget, earlier int) string {
	partial := ""
	if target.Partial {
		partial = " This review is partial: part of it could not be completed, so some files may not have been fully reviewed."
	}
	block := target.BlockSeverity
	if block == "" {
		block = overload.DefaultBlockSeverity
	}
	switch event {
	case overload.ReviewRequestChanges:
		if len(target.Findings) == 0 {
			return fmt.Sprintf("Issues reported on earlier commits at %s severity or above are still present.%s", block, partial)
		}
		return fmt.Sprintf("Overload found %d issue(s) in this pull request, including at least one at %s severity or above that should be fixed before merging.%s", len(target.Findings), block, partial)
	case overload.ReviewApprove:
		if len(target.Findings) == 0 && earlier == 0 {
			return "Overload found no issues in this pull request."
		}
		return fmt.Sprintf("Overload found nothing at %s severity or above. %d minor issue(s) are noted as comments.", block, len(target.Findings)+earlier)
	default:
		return fmt.Sprintf("Overload found %d issue(s) in this pull request.%s", len(target.Findings), partial)
	}
}
