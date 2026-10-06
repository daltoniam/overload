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
	PostReview(ctx context.Context, installationID int64, repository string, number int, sha, summary string, findings []overload.Finding) (int64, error)
}

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
	if len(target.Findings) == 0 {
		return w.Store.FinishPost(ctx, target, "nothing_new", 0)
	}
	summary := fmt.Sprintf("Overload found %d issue(s) in this pull request.\n\n%s", len(target.Findings), marker)
	if target.Partial {
		summary = fmt.Sprintf("Overload found %d issue(s) in this pull request. This review is partial: part of it could not be completed, so some files may not have been fully reviewed.\n\n%s", len(target.Findings), marker)
	}
	reviewID, err := poster.PostReview(ctx, target.InstallationID, target.Repository, target.PRNumber, target.HeadSHA, summary, target.Findings)
	if errors.Is(err, github.ErrReviewRejected) {
		return w.Store.RejectPost(ctx, runID, err.Error())
	}
	if err != nil {
		return fmt.Errorf("post review: %w", err)
	}
	return w.Store.FinishPost(ctx, target, "posted", reviewID)
}
