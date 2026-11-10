package queue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/harness"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
)

// defaultBakeoffReviewTimeout matches the default review timeout.
const defaultBakeoffReviewTimeout = 260 * time.Minute

var errBakeoffInterrupted = errors.New(postgres.BakeoffInterrupted)

// BakeoffWorker runs a bake-off: every pinned model reviews every case of
// every pinned suite, the given number of times, one review at a time.
// Reviews run in the server process even when pull request reviews use
// sandboxes: a case is a few files held in memory that the harness only
// reads, and no repository code or tool runs.
type BakeoffWorker struct {
	river.WorkerDefaults[postgres.BakeoffArgs]
	Store *postgres.Store
	// ReviewTimeout bounds each review; zero uses the default review
	// timeout.
	ReviewTimeout time.Duration
	// Review runs one review; nil uses the harness.
	Review func(context.Context, overload.ReviewSpec, fs.FS) (overload.ReviewResult, error)
}

// Timeout is unlimited because a bake-off runs many reviews, each bounded
// by ReviewTimeout. River treats a job running longer than the review
// timeout plus an hour as stuck and queues another attempt; that attempt
// finds the running attempt's lock taken and does nothing, so the first
// one carries on.
func (worker *BakeoffWorker) Timeout(*river.Job[postgres.BakeoffArgs]) time.Duration {
	return -1
}

func (worker *BakeoffWorker) Work(ctx context.Context, job *river.Job[postgres.BakeoffArgs]) error {
	plan, release, ok, err := worker.Store.StartBakeoff(ctx, job.Args.ID, job.ID)
	if err != nil || !ok {
		return err
	}
	defer release()
	review := worker.Review
	if review == nil {
		review = harness.Reviewer{}.Review
	}
	for _, model := range plan.Models {
		workflow, err := harness.ProfileWorkflow(model.ModelProfile, model.Prompt())
		if err != nil {
			return worker.finish(ctx, plan.ID, fmt.Errorf("model %s: %w", model.Name, err))
		}
		for _, suite := range plan.Suites {
			for _, item := range suite.Cases {
				for run := 1; run <= plan.Runs; run++ {
					if ctx.Err() != nil {
						return worker.finish(ctx, plan.ID, errBakeoffInterrupted)
					}
					// An attempt that took over the bake-off keeps the
					// reviews already recorded.
					if plan.Done[postgres.BakeoffResultKey(model.Name, suite.Name, item.Name, run)] {
						continue
					}
					result := postgres.BakeoffResult{Model: model.Name, Suite: suite.Name, Case: item.Name, Run: run, Score: eval.Score{Expected: eval.ScoreFindings(item.Expected, nil, 0).Expected}}
					// The policy can change during a bake-off that lasts
					// hours, so it is checked before every review.
					policy, err := worker.Store.LoadModelPolicy(ctx)
					if err != nil {
						if ctx.Err() != nil {
							return worker.finish(ctx, plan.ID, errBakeoffInterrupted)
						}
						return worker.finish(ctx, plan.ID, err)
					}
					if refused := policy.Check(model.Catalog, model.Model).Error(); refused != nil {
						result.Error = "refused by the model policy: " + refused.Error()
					} else {
						result = worker.runCase(ctx, review, workflow, item, result)
						// A review cut short by shutdown says nothing about
						// the model, so it is not recorded.
						if ctx.Err() != nil {
							return worker.finish(ctx, plan.ID, errBakeoffInterrupted)
						}
					}
					if err := worker.Store.AddBakeoffResult(context.WithoutCancel(ctx), plan.ID, result); err != nil {
						return worker.finish(ctx, plan.ID, err)
					}
				}
			}
		}
	}
	return worker.finish(ctx, plan.ID, nil)
}

// runCase reviews one case. A review that failed, or finished without
// reviewing every file (degraded), is recorded as failed: its findings
// would understate what the model finds.
func (worker *BakeoffWorker) runCase(ctx context.Context, review func(context.Context, overload.ReviewSpec, fs.FS) (overload.ReviewResult, error), workflow overload.ResolvedWorkflow, item eval.Case, result postgres.BakeoffResult) postgres.BakeoffResult {
	timeout := cmp.Or(worker.ReviewTimeout, defaultBakeoffReviewTimeout)
	reviewCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	reviewed, err := review(reviewCtx, overload.ReviewSpec{Diff: item.Diff, Workflow: workflow}, item.Repository())
	result.Duration = time.Since(started)
	result.InputTokens, result.OutputTokens = metricInt(reviewed.Metrics["input_tokens"]), metricInt(reviewed.Metrics["output_tokens"])
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(reviewCtx.Err(), context.DeadlineExceeded):
		result.Error = fmt.Sprintf("the review took longer than %s", timeout)
	case err != nil:
		result.Error = overload.TruncateUTF8(err.Error(), 500)
	case reviewed.Error != "":
		result.Error = overload.TruncateUTF8(reviewed.Error, 500)
	case isDegraded(reviewed):
		result.Error = overload.TruncateUTF8("review incomplete: "+degradedReason(reviewed), 500)
	default:
		result.Score = eval.ScoreFindings(item.Expected, reviewed.Findings, eval.LineTolerance)
	}
	return result
}

func isDegraded(result overload.ReviewResult) bool {
	degraded, _ := result.Metrics["degraded"].(bool)
	return degraded
}

func degradedReason(result overload.ReviewResult) string {
	if routing, ok := result.Metrics["routing"].(overload.Routing); ok && len(routing.Degraded) > 0 {
		return strings.Join(routing.Degraded, "; ")
	}
	return "not every file was reviewed"
}

func (worker *BakeoffWorker) finish(ctx context.Context, id int64, cause error) error {
	failure := ""
	if cause != nil {
		failure = cause.Error()
		slog.Warn("bake-off failed", "bakeoff_id", id, "error", cause)
	}
	if err := worker.Store.FinishBakeoff(context.WithoutCancel(ctx), id, failure); err != nil {
		return fmt.Errorf("finish bake-off: %w", err)
	}
	return nil
}

// metricInt reads a token count stored as any integer or float type.
func metricInt(value any) int64 {
	switch number := value.(type) {
	case int:
		return int64(number)
	case int64:
		return number
	case float64:
		return int64(number)
	default:
		return 0
	}
}
