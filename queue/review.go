package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/harness"
	"github.com/daltoniam/overload/postgres"
	reviewpkg "github.com/daltoniam/overload/review"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

type ReviewWorker struct {
	river.WorkerDefaults[postgres.ReviewArgs]
	Store   *postgres.Store
	Source  reviewpkg.PullRequestSource
	Sandbox overload.SandboxRunner
	Models  reviewpkg.ModelGrants
	// Status sets commit statuses; nil uses the GitHub App.
	Status StatusSetter
	// Archives caches head archives on disk; nil downloads every time.
	Archives *reviewpkg.ArchiveCache
}

func (w *ReviewWorker) Work(ctx context.Context, job *river.Job[postgres.ReviewArgs]) error {
	runID := job.Args.RunID
	tx, err := w.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&status); err != nil {
		return err
	}
	if status == "running" {
		if _, err := tx.Exec(ctx, `UPDATE runs SET status='failed',error_code='review_interrupted',error_message='Review worker stopped before completion',finished_at=now() WHERE id=$1`, runID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if status != "queued" {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'running', started_at = now() WHERE id = $1`, runID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'info', 'running', 'Review started')`, runID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	reportStatus(ctx, w.Store, w.Status, runID, statusPending, "Reviewing")
	run, resolved, repoName, err := w.Store.LoadRunWorkflow(ctx, runID)
	if err != nil || resolved.Kind != "pr_review" || resolved.Verify() != nil {
		return w.failRun(ctx, runID, "invalid_workflow", "Pinned PR workflow is unavailable")
	}
	source := w.Source
	if source == nil {
		var code string
		if source, code, err = sourceFor(ctx, w.Store, run.ForgeID, run.InstallationID); err != nil {
			return w.failRun(ctx, runID, code, err.Error()+" to worker")
		}
	}
	again := w.rereviewBase(ctx, run, resolved, source, repoName)
	if w.Archives != nil {
		source = reviewpkg.CachedSource{PullRequestSource: source, Cache: w.Archives}
	}
	var runner reviewpkg.Runner = reviewpkg.LocalRunner{Source: source, Reviewer: harness.Reviewer{}, Workflow: resolved, Rereview: again}
	if w.Sandbox != nil {
		runner = reviewpkg.SandboxRunner{Source: source, Sandbox: w.Sandbox, Workflow: resolved, RunID: runID, Models: w.Models, Rereview: again}
	}
	spec, result, sha, reviewErr := runner.Review(ctx, repoName, run.PRNumber)
	if spend, ok := overload.MetricsSpend(result.Metrics); ok {
		if err := w.Store.SetRunCost(context.WithoutCancel(ctx), runID, spend); err != nil {
			slog.Warn("run cost not recorded", "run_id", runID, "error", err)
		}
	}
	if name, ok := result.Metrics["sandbox"].(string); ok {
		if _, err := w.Store.Pool.Exec(ctx, `UPDATE runs SET sandbox_name=$2 WHERE id=$1`, runID, name); err != nil {
			return err
		}
	}
	if reviewErr == nil && sha != run.HeadSHA {
		reviewErr = fmt.Errorf("PR head changed before review")
	}
	if reviewErr != nil {
		slog.Error("PR review failed", "run_id", runID, "repository", repoName, "error", reviewErr)
		return w.failRun(ctx, runID, "review_failed", "PR review failed: "+reviewErr.Error())
	}
	if result.Error != "" {
		slog.Error("model review failed", "run_id", runID, "repository", repoName, "error", result.Error)
		return w.failRun(ctx, runID, "model_failed", "Model review failed: "+result.Error)
	}
	if err := w.finishReview(ctx, run, repoName, spec, result, sha); err != nil {
		return err
	}
	return nil
}

// replySource reads replies to overload's earlier review comments.
type replySource interface {
	PriorReplies(ctx context.Context, repository string, number int, prior []overload.PriorFinding) (map[int64][]string, error)
}

// rereviewBase prepares an incremental re-review when the workflow asks for
// one and the pull request has an earlier completed review with the same
// workflow revision. Replies to earlier findings are read when the source
// can; any failure falls back to a full review or no replies.
func (w *ReviewWorker) rereviewBase(ctx context.Context, run overload.Run, workflow overload.ResolvedWorkflow, source reviewpkg.PullRequestSource, repository string) *reviewpkg.Rereview {
	if !workflow.Rereview.IncrementalReviews() || run.ID == 0 {
		return nil
	}
	sinceID, previous, prior, err := w.Store.RereviewBase(ctx, run, workflow)
	if err != nil {
		slog.Warn("re-review base unavailable; reviewing in full", "run_id", run.ID, "error", err)
		return nil
	}
	if sinceID == 0 {
		return nil
	}
	if replies, ok := source.(replySource); ok && len(prior) > 0 {
		byFinding, err := replies.PriorReplies(ctx, repository, run.PRNumber, prior)
		if err != nil {
			slog.Warn("replies to earlier findings unavailable", "run_id", run.ID, "error", err)
		}
		for index := range prior {
			for _, reply := range byFinding[prior[index].ID] {
				prior[index].Replies = append(prior[index].Replies, reviewpkg.Sanitize(reply, 1000))
			}
		}
	}
	if _, err := w.Store.Pool.Exec(ctx, `UPDATE runs SET since_run_id=$2 WHERE id=$1`, run.ID, sinceID); err != nil {
		slog.Warn("re-review base not recorded", "run_id", run.ID, "error", err)
	}
	w.event(ctx, run.ID, fmt.Sprintf("Re-review of the changes since run %d", sinceID))
	return &reviewpkg.Rereview{SinceRunID: sinceID, PreviousDiff: previous, Prior: prior}
}

func (w *ReviewWorker) event(ctx context.Context, runID int64, message string) {
	if _, err := w.Store.Pool.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'info', 'rereview', $2)`, runID, message); err != nil {
		slog.Warn("run event not recorded", "run_id", runID, "error", err)
	}
}

// AppClient returns the GitHub App client from the environment or the
// App stored by the setup flow.
func AppClient(ctx context.Context, store *postgres.Store) (*github.Client, error) {
	return appClient(ctx, store)
}

func appClient(ctx context.Context, store *postgres.Store) (*github.Client, error) {
	if os.Getenv("GITHUB_APP_ID") != "" {
		return github.FromEnvironment()
	}
	app, err := store.LoadGitHubApp(ctx)
	if err != nil {
		return nil, err
	}
	return github.NewClient(app.AppID, []byte(app.PrivateKey))
}

func (w *ReviewWorker) failRun(ctx context.Context, runID int64, code, message string) error {
	message = overload.TruncateUTF8(message, 500)
	_, err := w.Store.Pool.Exec(context.WithoutCancel(ctx), `UPDATE runs SET status='failed',error_code=$2,error_message=$3,finished_at=now() WHERE id=$1 AND status='running'`, runID, code, message)
	if err == nil {
		_, err = w.Store.Pool.Exec(context.WithoutCancel(ctx), `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'error', 'failed', $2)`, runID, message)
	}
	reportStatus(ctx, w.Store, w.Status, runID, statusError, "Review failed: "+message)
	return err
}

func (w *ReviewWorker) finishReview(ctx context.Context, run overload.Run, repoName string, spec overload.ReviewSpec, result overload.ReviewResult, sha string) error {
	runID := run.ID
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	output, err := json.Marshal(result)
	if err != nil {
		return err
	}
	finalTx, err := w.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = finalTx.Rollback(ctx) }()
	metrics, err := json.Marshal(result.Metrics)
	if err != nil {
		return err
	}
	command, err := finalTx.Exec(ctx, `UPDATE runs SET status='completed', head_sha=$2, prompt_version=$3, metrics=$4, finished_at=now() WHERE id=$1 AND status='running' AND head_sha=$2`, runID, sha, spec.Workflow.Name, metrics)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	if command.RowsAffected() == 0 {
		return nil
	}
	if _, err := finalTx.Exec(ctx, `INSERT INTO run_artifacts(run_id,name,content) VALUES ($1,'spec.json',$2),($1,'result.json',$3)`, runID, data, output); err != nil {
		return err
	}
	plan := planPosting(run.DryRun, PostingEnabled())
	tierSkipped, _ := result.Metrics["tier_skipped"].(bool)
	if tierSkipped {
		reason, _ := result.Metrics["tier_reason"].(string)
		plan = postingPlan{findingStatus: "dry_run", postStatus: "tier_skipped", message: "Trivial change; not reviewed (" + reason + ")"}
	}
	if err := postgres.InsertFindings(ctx, finalTx, runID, repoName, run.PRNumber, plan.findingStatus, append(result.Findings, result.Dropped...)); err != nil {
		return err
	}
	if _, err := postgres.ResolveFindings(ctx, finalTx, runID, resolvedPrior(spec, result)); err != nil {
		return err
	}
	if plan.enqueue {
		client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
		if err != nil {
			return err
		}
		if _, err := client.InsertTx(ctx, finalTx, postgres.PostReviewArgs{RunID: runID}, nil); err != nil {
			return err
		}
	}
	if plan.postStatus != "" {
		if _, err := finalTx.Exec(ctx, `UPDATE runs SET post_status=$2 WHERE id=$1`, runID, plan.postStatus); err != nil {
			return err
		}
	}
	message := plan.message
	if _, err := finalTx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'info', 'completed', $2)`, runID, message); err != nil {
		return err
	}
	if err := finalTx.Commit(ctx); err != nil {
		return err
	}
	if tierSkipped {
		reportStatus(ctx, w.Store, w.Status, runID, statusSuccess, "Overload skipped: trivial change")
		return nil
	}
	if !plan.enqueue {
		degraded, _ := result.Metrics["degraded"].(bool)
		state, description := finalStatus(overload.ReviewComment, len(result.Findings), degraded)
		if plan.postStatus != "" {
			description += " (not posted: posting is off)"
		}
		reportStatus(ctx, w.Store, w.Status, runID, state, description)
	}
	return nil
}

type postingPlan struct {
	findingStatus string
	postStatus    string
	enqueue       bool
	message       string
}

// planPosting decides what happens to a completed review's findings: a
// dry-run repository never posts; otherwise posting is queued only when the
// server allows it, and the run records why it was not.
func planPosting(dryRun, enabled bool) postingPlan {
	switch {
	case dryRun:
		return postingPlan{findingStatus: "dry_run", message: "Dry-run PR review completed"}
	case enabled:
		return postingPlan{findingStatus: "pending", postStatus: "queued", enqueue: true, message: "PR review completed; posting queued"}
	default:
		return postingPlan{findingStatus: "dry_run", postStatus: "posting_disabled", message: "PR review completed; not posted because OVERLOAD_ENABLE_POSTING is not 1"}
	}
}

func NewClient(store *postgres.Store, concurrent int, timeoutSeconds int, sandbox overload.SandboxRunner, models reviewpkg.ModelGrants) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &ReviewWorker{Store: store, Sandbox: sandbox, Models: models, Archives: archiveCacheFromEnvironment()})
	river.AddWorker(workers, &ScheduleWorker{Store: store})
	river.AddWorker(workers, &PostWorker{Store: store})
	river.AddWorker(workers, &SkipStatusWorker{Store: store})
	return river.NewClient(riverpgxv5.New(store.Pool), &river.Config{
		Queues:     map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: concurrent}},
		Workers:    workers,
		JobTimeout: time.Duration(timeoutSeconds) * time.Second,
	})
}

// resolvedPrior returns the earlier findings a re-review judged fixed,
// among those it was shown.
func resolvedPrior(spec overload.ReviewSpec, result overload.ReviewResult) []int64 {
	if spec.Incremental == nil {
		return nil
	}
	shown := map[int64]bool{}
	for _, finding := range spec.Incremental.Prior {
		shown[finding.ID] = true
	}
	var ids []int64
	for _, verdict := range result.Prior {
		if verdict.Status == overload.PriorResolved && shown[verdict.ID] {
			ids = append(ids, verdict.ID)
		}
	}
	return ids
}

// archiveCacheFromEnvironment caches head archives in
// OVERLOAD_ARCHIVE_CACHE_DIR, up to OVERLOAD_ARCHIVE_CACHE_MB megabytes
// (default 2048), when the directory is set.
func archiveCacheFromEnvironment() *reviewpkg.ArchiveCache {
	dir := os.Getenv("OVERLOAD_ARCHIVE_CACHE_DIR")
	if dir == "" {
		return nil
	}
	megabytes := int64(2048)
	if value, err := strconv.ParseInt(os.Getenv("OVERLOAD_ARCHIVE_CACHE_MB"), 10, 64); err == nil && value > 0 {
		megabytes = value
	}
	return &reviewpkg.ArchiveCache{Dir: dir, MaxBytes: megabytes << 20}
}
