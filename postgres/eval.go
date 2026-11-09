package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// EvalTrigger marks runs queued by `overload eval prs`. They are dry runs:
// nothing is posted and no commit status is set.
const EvalTrigger = "eval"

// EvalRequest asks for one dry-run review of a pull request's head with a
// named workflow, as part of an evaluation batch.
type EvalRequest struct {
	Repository string
	// RepositoryID, when set, names the repository exactly; otherwise the
	// name must match exactly one repository overload knows.
	RepositoryID int64
	PR           int
	HeadSHA      string
	BaseSHA      string
	Workflow     string
	Batch        string
}

// RepositoryGitHubID returns a repository's GitHub ID, or 0 when overload
// has not learned it from the GitHub App.
func (s *Store) RepositoryGitHubID(ctx context.Context, repoID int64) (int64, error) {
	var githubID *int64
	if err := s.Pool.QueryRow(ctx, `SELECT github_id FROM repositories WHERE id=$1`, repoID).Scan(&githubID); err != nil {
		return 0, err
	}
	if githubID == nil {
		return 0, nil
	}
	return *githubID, nil
}

// evalRepository finds the evaluated repository and its App installation.
// A name that matches more than one repository (the same owner/name on
// different hosts) is refused rather than guessed.
func evalRepository(ctx context.Context, tx pgx.Tx, request EvalRequest) (int64, *int64, error) {
	rows, err := tx.Query(ctx, `SELECT id, installation_id FROM repositories WHERE full_name=$1 AND ($2::bigint = 0 OR id = $2) ORDER BY id LIMIT 2`, request.Repository, request.RepositoryID)
	if err != nil {
		return 0, nil, err
	}
	type match struct {
		id           int64
		installation *int64
	}
	matches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (match, error) {
		var found match
		err := row.Scan(&found.id, &found.installation)
		return found, err
	})
	if err != nil {
		return 0, nil, err
	}
	switch len(matches) {
	case 0:
		return 0, nil, fmt.Errorf("%s is not a repository overload knows; add it first", request.Repository)
	case 1:
		return matches[0].id, matches[0].installation, nil
	}
	return 0, nil, fmt.Errorf("%s matches more than one repository overload knows; name it by repository ID", request.Repository)
}

// QueueEvalRun pins the workflow and queues a dry-run review the same way
// a webhook does, through the same worker and sandbox path. The repository
// must be known to overload; its App installation (if any) is pinned.
func (s *Store) QueueEvalRun(ctx context.Context, client *river.Client[pgx.Tx], request EvalRequest) (int64, error) {
	if request.PR < 1 || request.HeadSHA == "" || request.BaseSHA == "" {
		return 0, errors.New("evaluation needs a pull request number and its head and base commits")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	repoID, installation, err := evalRepository(ctx, tx, request)
	if err != nil {
		return 0, err
	}
	resolved, err := resolveWorkflow(ctx, tx, request.Workflow)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("workflow %s does not exist or is disabled", request.Workflow)
	}
	if err != nil {
		return 0, err
	}
	if resolved.Kind != "pr_review" {
		return 0, fmt.Errorf("workflow %s is not a PR review workflow", request.Workflow)
	}
	if err := resolved.Verify(); err != nil {
		return 0, fmt.Errorf("workflow %s cannot run: %w", request.Workflow, err)
	}
	snapshot, err := json.Marshal(resolved)
	if err != nil {
		return 0, err
	}
	metrics, err := json.Marshal(map[string]any{"eval_batch": request.Batch})
	if err != nil {
		return 0, err
	}
	var runID int64
	if err := tx.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, dry_run, config_snapshot, installation_id, metrics) VALUES ($1, $2, $3, $4, $5, true, $6, $7, $8) RETURNING id`, repoID, request.PR, request.HeadSHA, request.BaseSHA, EvalTrigger, snapshot, installation, metrics).Scan(&runID); err != nil {
		return 0, err
	}
	job, err := client.InsertTx(ctx, tx, ReviewArgs{RunID: runID}, nil)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET river_job_id=$1 WHERE id=$2`, job.Job.ID, runID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'info', 'queued', $2)`, runID, "Queued for evaluation batch "+overload.TruncateUTF8(request.Batch, 100)); err != nil {
		return 0, err
	}
	return runID, tx.Commit(ctx)
}

// EvalRunState is what an evaluation needs to know about one of its runs.
type EvalRunState struct {
	ID       int64
	PR       int
	Workflow string
	Status   overload.RunStatus
	Error    string
	Seconds  float64
	Spend    overload.Spend
	Result   overload.ReviewResult
}

// Done reports whether the run has finished one way or another.
func (state EvalRunState) Done() bool {
	return state.Status == overload.RunCompleted || state.Status == overload.RunFailed || state.Status == overload.RunSuperseded
}

// EvalRuns reads the state of evaluation runs, with each finished run's
// result, in the order of ids.
func (s *Store) EvalRuns(ctx context.Context, ids []int64) ([]EvalRunState, error) {
	rows, err := s.Pool.Query(ctx, `SELECT r.id, r.pr_number, COALESCE(r.config_snapshot->>'name',''), r.status, COALESCE(r.error_message,''), COALESCE(EXTRACT(EPOCH FROM r.finished_at - r.started_at),0)::float8, COALESCE(r.cost_usd,0), r.cost_unpriced_calls, a.content FROM runs r LEFT JOIN run_artifacts a ON a.run_id=r.id AND a.name='result.json' WHERE r.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]EvalRunState{}
	for rows.Next() {
		var state EvalRunState
		var result []byte
		if err := rows.Scan(&state.ID, &state.PR, &state.Workflow, &state.Status, &state.Error, &state.Seconds, &state.Spend.USD, &state.Spend.Unpriced, &result); err != nil {
			return nil, err
		}
		if len(result) > 0 {
			if err := json.Unmarshal(result, &state.Result); err != nil {
				return nil, fmt.Errorf("run %d result: %w", state.ID, err)
			}
		}
		byID[state.ID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	states := make([]EvalRunState, 0, len(ids))
	for _, id := range ids {
		state, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("evaluation run %d is missing", id)
		}
		states = append(states, state)
	}
	return states, nil
}
