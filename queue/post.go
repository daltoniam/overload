package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
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

// Work posts a run's review. When the job's last attempt fails too, the
// post is given up with a failing verdict and status, so the run does not
// stay waiting to be posted and holding back automated re-reviews.
func (w *PostWorker) Work(ctx context.Context, job *river.Job[postgres.PostReviewArgs]) error {
	err := w.postRecovering(ctx, job.Args.RunID)
	if err != nil && job.Attempt >= job.MaxAttempts {
		w.giveUp(context.WithoutCancel(ctx), job.Args.RunID, err)
	}
	return err
}

// postRecovering posts a run's review, turning a panic into an error so a
// panicking last attempt still gives up instead of leaving the post queued.
func (w *PostWorker) postRecovering(ctx context.Context, runID int64) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("post review panicked: %v", recovered)
		}
	}()
	return w.post(ctx, runID)
}

func (w *PostWorker) giveUp(ctx context.Context, runID int64, cause error) {
	if err := w.Store.FailPost(ctx, runID, cause.Error()); err != nil {
		slog.Error("failed post not recorded", "run_id", runID, "error", err)
		return
	}
	reportStatus(ctx, w.Store, w.Status, runID, overload.GateFailure, "Overload could not finish posting its review; review the pull request again")
}

func (w *PostWorker) post(ctx context.Context, runID int64) error {
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
		if err := w.Store.RecordGate(ctx, runID, gate); err != nil {
			return err
		}
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
	notes := gateNotes(gate, target.History)
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

// finish withdraws stale reviews, records the verdict and the post, and
// sets the commit status. The verdict is recorded before the post, because
// a recorded post is never retried.
func (w *PostWorker) finish(ctx context.Context, poster ReviewPoster, target postgres.PostTarget, gate overload.Gate, standing []github.StandingReview, postStatus string, reviewID int64) error {
	if err := w.dismiss(ctx, poster, target, gate, standing, reviewID); err != nil {
		return err
	}
	if err := w.Store.RecordGate(ctx, target.RunID, gate); err != nil {
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

// gateNotes explains a failing or held gate on the pull request: changed
// files that could not be read or were not reviewed, an earlier failing
// verdict this review cannot clear, and sensitive files that still need a
// person. The earlier verdict is explained only once, not on every held
// re-review, and the sensitive files only when they differ from those of
// overload's last posted review, so repeated reviews do not repeat them.
func gateNotes(gate overload.Gate, history postgres.GateHistory) string {
	var notes strings.Builder
	if gate.FilesUnknown {
		notes.WriteString("\n\nOverload could not read this pull request's changed files, so the overload check fails until the pull request is reviewed again.")
	}
	if len(gate.Unreviewed) > 0 {
		fmt.Fprintf(&notes, "\n\nNot reviewed, so the overload check fails for this commit: %s.", codeList(gate.Unreviewed))
	}
	if gate.HeldBack && !history.PreviousHeld {
		notes.WriteString("\n\nAn earlier overload review of this commit failed the check. A review requested through the API or by automation cannot clear that; a person can review the commit again from overload's UI or CLI.")
	}
	if len(gate.HumanFiles) > 0 && (!history.HumanFilesKnown || !sameFiles(history.PostedHumanFiles, gate.HumanFiles)) {
		fmt.Fprintf(&notes, "\n\nThese files are on this workflow's human review paths, so overload does not approve this pull request: %s.", codeList(gate.HumanFiles))
	}
	return notes.String()
}

func sameFiles(first, second []string) bool {
	first, second = slices.Clone(first), slices.Clone(second)
	slices.Sort(first)
	slices.Sort(second)
	return slices.Equal(first, second)
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
// changes or is held by an earlier failing verdict. GitHub counts each
// reviewer's latest approval or request and a comment replaces neither, so
// a stale approval would otherwise keep counting toward the merge; when one
// cannot be withdrawn, the status fails before the job is retried.
func (w *PostWorker) dismiss(ctx context.Context, poster ReviewPoster, target postgres.PostTarget, gate overload.Gate, standing []github.StandingReview, posted int64) error {
	decides := target.ReviewDecision == overload.ReviewDecisionRequestChanges || target.ReviewDecision == overload.ReviewDecisionApprove
	for _, review := range standing {
		var message string
		switch {
		case review.ID == posted:
			continue
		case review.State == github.ReviewStateApproved && gate.Event != overload.ReviewApprove:
			message = withdrawApprovalMessage
		case review.State == github.ReviewStateChangesRequested && decides && gate.Event != overload.ReviewRequestChanges && !gate.Held:
			message = dismissMessage
		default:
			continue
		}
		if err := poster.DismissReview(ctx, target.InstallationID, target.Repository, target.PRNumber, review.ID, message); err != nil {
			if review.State == github.ReviewStateApproved {
				reportStatus(ctx, w.Store, w.Status, target.RunID, overload.GateFailure, "Could not withdraw overload's earlier approval")
			}
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
