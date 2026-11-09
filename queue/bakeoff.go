package queue

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/harness"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
)

// bakeoffTolerance is how many lines away from the expected range a
// finding may cite and still count; models count lines from hunk headers.
const bakeoffTolerance = 2

// BakeoffWorker runs a bake-off: every pinned model reviews every case of
// every pinned suite, the given number of times, one review at a time.
type BakeoffWorker struct {
	river.WorkerDefaults[postgres.BakeoffArgs]
	Store *postgres.Store
	// Review runs one review; nil uses the harness.
	Review func(context.Context, overload.ReviewSpec, fs.FS) (overload.ReviewResult, error)
}

// Timeout allows long bake-offs: a local model can take most of an hour per
// file.
func (worker *BakeoffWorker) Timeout(*river.Job[postgres.BakeoffArgs]) time.Duration {
	return 48 * time.Hour
}

func (worker *BakeoffWorker) Work(ctx context.Context, job *river.Job[postgres.BakeoffArgs]) error {
	bakeoff, ok, err := worker.Store.StartBakeoff(ctx, job.Args.ID)
	if err != nil || !ok {
		return err
	}
	review := worker.Review
	if review == nil {
		review = harness.Reviewer{}.Review
	}
	for _, model := range bakeoff.Models {
		policy, err := worker.Store.LoadModelPolicy(ctx)
		if err != nil {
			return worker.finish(ctx, bakeoff.ID, err)
		}
		refused := policy.Check(model.Catalog, model.Model).Error()
		workflow, err := harness.ProfileWorkflow(model, "context")
		if err != nil {
			return worker.finish(ctx, bakeoff.ID, err)
		}
		for _, suite := range bakeoff.Suites {
			for _, item := range suite.Cases {
				for run := 1; run <= bakeoff.Runs; run++ {
					if ctx.Err() != nil {
						return worker.finish(ctx, bakeoff.ID, ctx.Err())
					}
					result := postgres.BakeoffResult{Model: model.Name, Suite: suite.Name, Case: item.Name, Run: run, Score: eval.Score{Expected: len(item.Expected)}}
					if refused != nil {
						result.Error = "refused by the model policy: " + refused.Error()
					} else {
						result = worker.runCase(ctx, review, workflow, item, result)
					}
					if err := worker.Store.AddBakeoffResult(context.WithoutCancel(ctx), bakeoff.ID, result); err != nil {
						return worker.finish(ctx, bakeoff.ID, err)
					}
				}
			}
		}
	}
	return worker.finish(ctx, bakeoff.ID, nil)
}

func (worker *BakeoffWorker) runCase(ctx context.Context, review func(context.Context, overload.ReviewSpec, fs.FS) (overload.ReviewResult, error), workflow overload.ResolvedWorkflow, item eval.Case, result postgres.BakeoffResult) postgres.BakeoffResult {
	started := time.Now()
	reviewed, err := review(ctx, overload.ReviewSpec{Diff: item.Diff, Workflow: workflow}, item.Repository())
	result.Duration = time.Since(started)
	if err != nil {
		result.Error = overload.TruncateUTF8(err.Error(), 500)
		return result
	}
	result.Score = eval.ScoreFindings(item.Expected, reviewed.Findings, bakeoffTolerance)
	result.InputTokens, result.OutputTokens = metricInt(reviewed.Metrics["input_tokens"]), metricInt(reviewed.Metrics["output_tokens"])
	return result
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
