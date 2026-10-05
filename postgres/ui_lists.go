package postgres

import (
	"context"
	"strings"

	"github.com/daltoniam/overload"
)

type ListFilter struct {
	Query  string
	Kind   string
	Status string
	Page   int
}

func (filter ListFilter) normalized() ListFilter {
	filter.Query = strings.TrimSpace(filter.Query)
	if len(filter.Query) > 200 {
		filter.Query = filter.Query[:200]
	}
	filter.Page = min(max(filter.Page, 1), 100000)
	return filter
}

func literalPattern(query string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(query) + "%"
}

func (s *Store) SearchRuns(ctx context.Context, filter ListFilter) ([]overload.Run, int, error) {
	filter = filter.normalized()
	pattern := literalPattern(filter.Query)
	where := ` WHERE ($1 = '%%' OR id::text ILIKE $1 OR COALESCE(pr_number,0)::text ILIKE $1 OR trigger ILIKE $1 OR head_sha ILIKE $1) AND ($2 = '' OR kind = $2) AND ($3 = '' OR status = $3)`
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM runs`+where, pattern, filter.Kind, filter.Status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, COALESCE(repository_id,0), COALESCE(pr_number,0), head_sha, base_sha, trigger, kind, status, mode, dry_run, COALESCE(river_job_id,0), created_at, started_at, finished_at, error_code, error_message FROM runs`+where+` ORDER BY created_at DESC, id DESC LIMIT 25 OFFSET $4`, pattern, filter.Kind, filter.Status, (filter.Page-1)*25)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var runs []overload.Run
	for rows.Next() {
		var run overload.Run
		if err := rows.Scan(&run.ID, &run.RepositoryID, &run.PRNumber, &run.HeadSHA, &run.BaseSHA, &run.Trigger, &run.Kind, &run.Status, &run.Mode, &run.DryRun, &run.RiverJobID, &run.CreatedAt, &run.StartedAt, &run.FinishedAt, &run.ErrorCode, &run.ErrorMessage); err != nil {
			return nil, 0, err
		}
		runs = append(runs, run)
	}
	return runs, total, rows.Err()
}

func (s *Store) SearchDeliveries(ctx context.Context, filter ListFilter) ([]Delivery, int, error) {
	filter = filter.normalized()
	pattern := literalPattern(filter.Query)
	where := ` WHERE ($1 = '%%' OR repository_full_name ILIKE $1 OR event ILIKE $1 OR action ILIKE $1 OR skip_reason ILIKE $1) AND ($2 = '' OR event = $2) AND ($3 = '' OR outcome = $3)`
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries`+where, pattern, filter.Kind, filter.Status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT event, action, repository_full_name, outcome, skip_reason, received_at FROM webhook_deliveries`+where+` ORDER BY received_at DESC, id DESC LIMIT 25 OFFSET $4`, pattern, filter.Kind, filter.Status, (filter.Page-1)*25)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var deliveries []Delivery
	for rows.Next() {
		var delivery Delivery
		if err := rows.Scan(&delivery.Event, &delivery.Action, &delivery.Repository, &delivery.Outcome, &delivery.SkipReason, &delivery.ReceivedAt); err != nil {
			return nil, 0, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, total, rows.Err()
}
