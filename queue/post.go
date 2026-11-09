package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// Status sets commit statuses; nil uses the GitHub App.
	Status StatusSetter
}

func (w *PostWorker) Work(ctx context.Context, job *river.Job[postgres.PostReviewArgs]) error {
	runID := job.Args.RunID
	if !PostingEnabled() {
		if err := w.Store.SkipPost(ctx, runID, "posting_disabled"); err != nil {
			return err
		}
		reportStatus(ctx, w.Store, w.Status, runID, statusSuccess, "Reviewed; not posted because posting is off")
		return nil
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
		// The review was posted but the job stopped before recording it,
		// so the threads of fixed findings were never resolved either.
		if err := w.Store.FinishPost(ctx, target, "posted", existing); err != nil {
			return err
		}
		w.recordComments(ctx, poster, target, existing)
		w.resolveThreads(ctx, poster, target)
		return nil
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
		if err := w.Store.FinishPost(ctx, target, "nothing_new", 0); err != nil {
			return err
		}
		w.resolveThreads(ctx, poster, target)
		state, description := finalStatus(event, len(severities), target.Partial)
		reportStatus(ctx, w.Store, w.Status, runID, state, description)
		return nil
	}
	summary := reviewSummary(event, target, len(severities)-len(target.Findings)) + "\n\n" + marker
	reviewID, err := poster.PostReview(ctx, target.InstallationID, target.Repository, target.PRNumber, target.HeadSHA, event, summary, target.Findings)
	if errors.Is(err, github.ErrReviewRejected) {
		if err := w.Store.RejectPost(ctx, runID, err.Error()); err != nil {
			return err
		}
		reportStatus(ctx, w.Store, w.Status, runID, statusError, "GitHub rejected the review")
		return nil
	}
	if err != nil {
		return fmt.Errorf("post review: %w", err)
	}
	if err := w.dismiss(ctx, poster, target, event, standing); err != nil {
		return err
	}
	if err := w.Store.FinishPost(ctx, target, "posted", reviewID); err != nil {
		return err
	}
	w.recordComments(ctx, poster, target, reviewID)
	w.resolveThreads(ctx, poster, target)
	state, description := finalStatus(event, len(severities), target.Partial)
	reportStatus(ctx, w.Store, w.Status, runID, state, description)
	return nil
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

// commentLister lists the comments of a posted review.
type commentLister interface {
	ReviewComments(ctx context.Context, installationID int64, repository string, number int, reviewID int64) ([]github.PostedComment, error)
}

// recordComments saves the GitHub comment each posted finding became, so
// later replies and thread resolution follow the comment itself. It is
// best effort: without it, findings are matched by path, line and title.
func (w *PostWorker) recordComments(ctx context.Context, poster ReviewPoster, target postgres.PostTarget, reviewID int64) {
	lister, ok := poster.(commentLister)
	if !ok || reviewID <= 0 || len(target.Findings) == 0 {
		return
	}
	comments, err := lister.ReviewComments(ctx, target.InstallationID, target.Repository, target.PRNumber, reviewID)
	if err == nil {
		err = w.Store.SetFindingComments(context.WithoutCancel(ctx), github.MatchComments(target.Findings, comments))
	}
	if err != nil {
		slog.Warn("review comment IDs not recorded", "run_id", target.RunID, "error", err)
	}
}

// threadResolver resolves overload's review threads for fixed findings.
type threadResolver interface {
	ResolveThreads(ctx context.Context, installationID int64, repository string, number int, fixed []overload.Finding) (int, error)
}

// resolveThreads resolves the GitHub threads of findings a re-review found
// fixed, when the workflow asks for it. It is best effort: a failure is
// logged and recorded on the run, never retried.
func (w *PostWorker) resolveThreads(ctx context.Context, poster ReviewPoster, target postgres.PostTarget) {
	if !target.ResolveThreads || len(target.Fixed) == 0 {
		return
	}
	resolver, ok := poster.(threadResolver)
	if !ok {
		return
	}
	resolved, err := resolver.ResolveThreads(ctx, target.InstallationID, target.Repository, target.PRNumber, target.Fixed)
	message := fmt.Sprintf("Resolved %s for findings this review found fixed", overload.Plural(resolved, "review thread"))
	level := "info"
	if err != nil {
		slog.Warn("review threads not resolved", "run_id", target.RunID, "error", err)
		message, level = "Review threads not resolved: "+overload.TruncateUTF8(err.Error(), 200), "warn"
	}
	if _, err := w.Store.Pool.Exec(context.WithoutCancel(ctx), `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, $2, 'posted', $3)`, target.RunID, level, message); err != nil {
		slog.Warn("run event not recorded", "run_id", target.RunID, "error", err)
	}
}
