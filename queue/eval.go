package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// EvalPlan runs every workflow on the current head of every pull request
// as dry runs. Concurrency limits how many of the batch's runs are queued
// or running at once (the server's own worker limit still applies).
type EvalPlan struct {
	// Repository is the evaluated repository; its pull requests are read
	// from the host it is on.
	Repository overload.Repository
	// HostRepoID, when known, is the host's ID for the repository (its
	// GitHub ID or GitLab/Forgejo repository ID): every pull request read
	// must target it, so a renamed or same-named repository is never
	// evaluated by mistake.
	HostRepoID  int64
	PRs         []int
	Workflows   []string
	Concurrency int
	Batch       string
	Poll        time.Duration
	Progress    func(string)
}

type evalJob struct {
	pr       int
	workflow string
	head     string
	base     string
}

// RunEval queues the plan's runs a few at a time, waits for all of them,
// and returns their results in plan order. A pull request that cannot be
// read stops the evaluation before anything is queued.
func RunEval(ctx context.Context, store *postgres.Store, client *river.Client[pgx.Tx], reader PullRequestReader, plan EvalPlan) ([]eval.RunResult, error) {
	if len(plan.PRs) == 0 || len(plan.Workflows) == 0 {
		return nil, errors.New("evaluation needs pull requests and workflows")
	}
	progress := plan.Progress
	if progress == nil {
		progress = func(string) {}
	}
	poll := plan.Poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	var jobs []evalJob
	for _, number := range plan.PRs {
		pr, err := reader.PullRequest(ctx, plan.Repository, number)
		if err != nil {
			return nil, fmt.Errorf("read #%d: %w", number, err)
		}
		if plan.HostRepoID != 0 && pr.RepositoryID != plan.HostRepoID {
			return nil, fmt.Errorf("#%d is not a pull request of the repository overload knows as %s", number, plan.Repository.Ref())
		}
		head, base := pr.HeadSHA, pr.BaseSHA
		if head == "" || base == "" {
			return nil, fmt.Errorf("#%d has no head or base commit", number)
		}
		for _, workflow := range plan.Workflows {
			jobs = append(jobs, evalJob{pr: number, workflow: workflow, head: head, base: base})
		}
	}
	ids := make([]int64, len(jobs))
	limit := max(plan.Concurrency, 1)
	next, active := 0, map[int]int64{}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		for next < len(jobs) && len(active) < limit {
			job := jobs[next]
			id, err := store.QueueEvalRun(ctx, client, postgres.EvalRequest{Repository: plan.Repository.FullName, RepositoryID: plan.Repository.ID, PR: job.pr, HeadSHA: job.head, BaseSHA: job.base, Workflow: job.workflow, Batch: plan.Batch})
			if err != nil {
				return nil, fmt.Errorf("queue %s on #%d: %w", job.workflow, job.pr, err)
			}
			ids[next], active[next] = id, id
			progress(fmt.Sprintf("queued run %d: %s on #%d", id, job.workflow, job.pr))
			next++
		}
		if len(active) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		positions := make([]int, 0, len(active))
		running := make([]int64, 0, len(active))
		for position, id := range active {
			positions = append(positions, position)
			running = append(running, id)
		}
		states, err := store.EvalRuns(ctx, running)
		if err != nil {
			return nil, err
		}
		for index, state := range states {
			if state.Done() {
				delete(active, positions[index])
				progress(fmt.Sprintf("run %d %s: %s on #%d", state.ID, state.Status, state.Workflow, state.PR))
			}
		}
	}
	states, err := store.EvalRuns(ctx, ids)
	if err != nil {
		return nil, err
	}
	results := make([]eval.RunResult, 0, len(states))
	for index, state := range states {
		result := eval.RunResult{PR: jobs[index].pr, Workflow: jobs[index].workflow, RunID: state.ID, Status: string(state.Status), Error: state.Error, Findings: state.Result.Findings, Dropped: len(state.Result.Dropped), CostUSD: state.Spend.USD, Unpriced: state.Spend.Unpriced, Seconds: state.Seconds}
		if result.Findings == nil {
			result.Findings = []overload.Finding{}
		}
		if tier, ok := state.Result.Metrics["tier"].(string); ok {
			result.Tier = tier
		}
		result.Degraded = degradedReasons(state.Result.Metrics)
		results = append(results, result)
	}
	return results, nil
}

// degradedReasons reads why a review was partial from its metrics, which
// may have been decoded from JSON.
func degradedReasons(metrics map[string]any) []string {
	routing, ok := metrics["routing"].(map[string]any)
	if !ok {
		return nil
	}
	values, _ := routing["degraded"].([]any)
	var reasons []string
	for _, value := range values {
		if reason, ok := value.(string); ok {
			reasons = append(reasons, reason)
		}
	}
	return reasons
}
