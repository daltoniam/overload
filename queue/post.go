package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

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
	StandingReviews(ctx context.Context, installationID int64, repository string, number int) ([]github.StandingReview, error)
	DismissReview(ctx context.Context, installationID int64, repository string, number int, reviewID int64, message string) error
}

// Messages explaining why overload withdrew one of its own reviews.
const (
	dismissMessage          = "Overload's latest review of this pull request found nothing blocking."
	withdrawApprovalMessage = "Overload's latest review of this pull request does not approve it."
)

type PostWorker struct {
	river.WorkerDefaults[postgres.PostReviewArgs]
	Store  *postgres.Store
	Poster ReviewPoster
	// Status sets commit statuses; nil uses the GitHub App.
	Status StatusSetter
}

func (w *PostWorker) Work(ctx context.Context, job *river.Job[postgres.PostReviewArgs]) error {
	runID := job.Args.RunID
	target, err := w.Store.LoadPostTarget(ctx, runID)
	if errors.Is(err, postgres.ErrNothingToPost) {
		return nil
	}
	if err != nil {
		return err
	}
	if target.RerunOfHead {
		return w.Store.SkipPost(ctx, runID, "superseded_by_newer_run")
	}
	input := target.GateInput()
	if !PostingEnabled() {
		if err := w.Store.SkipPost(ctx, runID, "posting_disabled"); err != nil {
			return err
		}
		input.NotPosted = true
		gate := overload.GateOutcome(input)
		reportStatus(ctx, w.Store, w.Status, runID, gate.State, gate.Description)
		return nil
	}
	gate := overload.GateOutcome(input)
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
	standing, err := poster.StandingReviews(ctx, target.InstallationID, target.Repository, target.PRNumber)
	if err != nil {
		return fmt.Errorf("check earlier reviews: %w", err)
	}
	event := gate.Event
	if existing > 0 {
		// An earlier attempt posted the review and stopped before
		// withdrawing stale reviews and setting the status.
		return w.finish(ctx, poster, target, gate, standing, "posted", existing)
	}
	notes := gateNotes(gate)
	if len(target.Findings) == 0 && notes == "" && event != overload.ReviewApprove && (event != overload.ReviewRequestChanges || requestsChanges(standing)) {
		return w.finish(ctx, poster, target, gate, standing, "nothing_new", 0)
	}
	summary := reviewSummary(event, target, len(target.DuplicateSeverities)) + notes + "\n\n" + marker
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
	return w.finish(ctx, poster, target, gate, standing, "posted", reviewID)
}

// finish withdraws stale reviews, records the post and sets the commit
// status.
func (w *PostWorker) finish(ctx context.Context, poster ReviewPoster, target postgres.PostTarget, gate overload.Gate, standing []github.StandingReview, postStatus string, reviewID int64) error {
	if err := w.dismiss(ctx, poster, target, gate.Event, standing, reviewID); err != nil {
		return err
	}
	if err := w.Store.FinishPost(ctx, target, postStatus, reviewID); err != nil {
		return err
	}
	reportStatus(ctx, w.Store, w.Status, target.RunID, gate.State, gate.Description)
	return nil
}

func requestsChanges(standing []github.StandingReview) bool {
	for _, review := range standing {
		if review.State == github.ReviewStateChangesRequested {
			return true
		}
	}
	return false
}

// gateNotes tells reviewers which files the review did not cover and which
// sensitive files still need a person.
func gateNotes(gate overload.Gate) string {
	var notes strings.Builder
	if len(gate.Unreviewed) > 0 {
		fmt.Fprintf(&notes, "\n\nNot reviewed, so the overload check fails for this commit: %s.", codeList(gate.Unreviewed))
	}
	if len(gate.HumanFiles) > 0 {
		fmt.Fprintf(&notes, "\n\nThese files are on this workflow's human review paths, so overload does not approve this pull request: %s.", codeList(gate.HumanFiles))
	}
	return notes.String()
}

// codeList formats up to ten paths for a review body. Paths come from the
// pull request, so control characters that could end the code span or
// start new markdown are replaced, and mentions are broken up as in model
// text.
func codeList(paths []string) string {
	shown := paths
	if len(shown) > 10 {
		shown = shown[:10]
	}
	quoted := make([]string, len(shown))
	for index, path := range shown {
		path = strings.Map(func(r rune) rune {
			if r == '`' {
				return '\''
			}
			if unicode.IsControl(r) {
				return '?'
			}
			return r
		}, overload.TruncateUTF8(path, 200))
		quoted[index] = "`" + strings.ReplaceAll(path, "@", "@ ") + "`"
	}
	list := strings.Join(quoted, ", ")
	if len(paths) > len(shown) {
		list += fmt.Sprintf(" and %d more", len(paths)-len(shown))
	}
	return list
}

// dismiss withdraws overload's earlier reviews that the new one no longer
// stands behind: an approval unless this review approves, and, when the
// workflow decides, a request for changes unless this review requests
// changes. GitHub counts each reviewer's latest approval or request and a
// comment replaces neither, so a stale approval would otherwise keep
// counting toward the merge.
func (w *PostWorker) dismiss(ctx context.Context, poster ReviewPoster, target postgres.PostTarget, event string, standing []github.StandingReview, posted int64) error {
	decides := target.ReviewDecision == overload.ReviewDecisionRequestChanges || target.ReviewDecision == overload.ReviewDecisionApprove
	for _, review := range standing {
		var message string
		switch {
		case review.ID == posted:
			continue
		case review.State == github.ReviewStateApproved && event != overload.ReviewApprove:
			message = withdrawApprovalMessage
		case review.State == github.ReviewStateChangesRequested && decides && event != overload.ReviewRequestChanges:
			message = dismissMessage
		default:
			continue
		}
		if err := poster.DismissReview(ctx, target.InstallationID, target.Repository, target.PRNumber, review.ID, message); err != nil {
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
