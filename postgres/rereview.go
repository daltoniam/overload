package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// maxPriorFindings bounds the earlier findings a re-review is shown.
const maxPriorFindings = 50

// RereviewBase finds what an incremental re-review of a run builds on: the
// latest earlier completed review of the same pull request with the same
// workflow revision (its diff), and the pull request's open findings. It
// returns 0 when there is no such review.
func (s *Store) RereviewBase(ctx context.Context, run overload.Run, workflow overload.ResolvedWorkflow) (int64, string, []overload.PriorFinding, error) {
	var sinceID int64
	var specData []byte
	err := s.Pool.QueryRow(ctx, `SELECT r.id, a.content FROM runs r JOIN run_artifacts a ON a.run_id=r.id AND a.name='spec.json' WHERE r.repository_id=$1 AND r.pr_number=$2 AND r.id<$3 AND r.status='completed' AND r.kind='pr_review' AND r.trigger<>$4 AND r.config_snapshot->>'name'=$5 AND (r.config_snapshot->>'revision')::int=$6 ORDER BY r.id DESC LIMIT 1`, run.RepositoryID, run.PRNumber, run.ID, EvalTrigger, workflow.Name, workflow.Revision).Scan(&sinceID, &specData)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil, nil
	}
	if err != nil {
		return 0, "", nil, err
	}
	var spec overload.ReviewSpec
	if err := json.Unmarshal(specData, &spec); err != nil {
		return 0, "", nil, err
	}
	// The latest finding per fingerprint decides whether an issue is
	// open, so a fixed issue does not come back through an older copy.
	rows, err := s.Pool.Query(ctx, `SELECT id, path, line, severity, title, body FROM (SELECT DISTINCT ON (f.fingerprint) f.id, f.path, f.line, f.severity, f.title, f.body, f.resolved_by_run_id FROM findings f JOIN runs r ON r.id=f.run_id WHERE r.repository_id=$1 AND r.pr_number=$2 AND r.id<$3 AND r.trigger<>$4 AND f.status IN ('posted', 'dry_run', 'suppressed') ORDER BY f.fingerprint, f.id DESC) latest WHERE resolved_by_run_id IS NULL ORDER BY id DESC LIMIT $5`, run.RepositoryID, run.PRNumber, run.ID, EvalTrigger, maxPriorFindings)
	if err != nil {
		return 0, "", nil, err
	}
	defer rows.Close()
	var prior []overload.PriorFinding
	for rows.Next() {
		var finding overload.PriorFinding
		if err := rows.Scan(&finding.ID, &finding.Path, &finding.Line, &finding.Severity, &finding.Title, &finding.Body); err != nil {
			return 0, "", nil, err
		}
		prior = append(prior, finding)
	}
	return sinceID, spec.Diff, prior, rows.Err()
}

// ResolveFindings marks earlier findings of a run's pull request as fixed
// by the run, and returns the ones it marked.
func ResolveFindings(ctx context.Context, tx pgx.Tx, runID int64, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Every earlier copy of a fixed finding (same fingerprint on the same
	// pull request) is resolved with it.
	rows, err := tx.Query(ctx, `UPDATE findings f SET resolved_by_run_id=$1 FROM runs r, runs current WHERE current.id=$1 AND r.id=f.run_id AND r.repository_id=current.repository_id AND r.pr_number=current.pr_number AND r.id < $1 AND f.resolved_by_run_id IS NULL AND f.fingerprint IN (SELECT p.fingerprint FROM findings p JOIN runs pr ON pr.id=p.run_id WHERE p.id = ANY($2) AND pr.repository_id=current.repository_id AND pr.pr_number=current.pr_number) RETURNING f.id`, runID, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}
