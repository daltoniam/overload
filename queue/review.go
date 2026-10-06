package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
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
	run, resolved, repoName, err := w.Store.LoadRunWorkflow(ctx, runID)
	if err != nil || resolved.Kind != "pr_review" || resolved.Verify() != nil {
		return w.failRun(ctx, runID, "invalid_workflow", "Pinned PR workflow is unavailable")
	}
	source := w.Source
	if source == nil {
		if run.InstallationID > 0 {
			client, err := appClient(ctx, w.Store)
			if err != nil {
				return w.failRun(ctx, runID, "missing_github_app", "GitHub App credentials unavailable to worker")
			}
			source = github.InstallationSource{Client: client, InstallationID: run.InstallationID}
		}
	}
	if source == nil {
		token := github.TokenFromEnvironment()
		if token == "" {
			return w.failRun(ctx, runID, "missing_github_credentials", "GitHub credentials unavailable to worker")
		}
		client, err := github.NewTokenClient(token)
		if err != nil {
			return w.failRun(ctx, runID, "github_client", "GitHub client unavailable")
		}
		source = client
	}
	var runner reviewpkg.Runner = reviewpkg.LocalRunner{Source: source, Reviewer: harness.Reviewer{}, Workflow: resolved}
	if w.Sandbox != nil {
		runner = reviewpkg.SandboxRunner{Source: source, Sandbox: w.Sandbox, Workflow: resolved, RunID: runID}
	}
	spec, result, sha, reviewErr := runner.Review(ctx, repoName, run.PRNumber)
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
		return w.failRun(ctx, runID, "review_failed", "PR review failed; inspect worker logs")
	}
	if result.Error != "" {
		slog.Error("model review failed", "run_id", runID, "repository", repoName, "error", result.Error)
		return w.failRun(ctx, runID, "model_failed", "Model review failed")
	}
	if err := w.finishReview(ctx, run, repoName, spec, result, sha); err != nil {
		return err
	}
	return nil
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
	if err := postgres.InsertFindings(ctx, finalTx, runID, repoName, run.PRNumber, plan.findingStatus, append(result.Findings, result.Dropped...)); err != nil {
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
	return finalTx.Commit(ctx)
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

func NewClient(store *postgres.Store, concurrent int, timeoutSeconds int, sandbox overload.SandboxRunner) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &ReviewWorker{Store: store, Sandbox: sandbox})
	river.AddWorker(workers, &ScheduleWorker{Store: store})
	river.AddWorker(workers, &PostWorker{Store: store})
	return river.NewClient(riverpgxv5.New(store.Pool), &river.Config{
		Queues:     map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: concurrent}},
		Workers:    workers,
		JobTimeout: time.Duration(timeoutSeconds) * time.Second,
	})
}
