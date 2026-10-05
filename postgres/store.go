package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// Migrate brings River's and overload's schema up to date. A session
// advisory lock serializes concurrent callers, such as several processes or
// test packages starting against one fresh database, which would otherwise
// race to create the same objects.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(729139)`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(729139)`); err != nil {
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()
	migrator, err := rivermigrate.New(riverpgxv5.New(s.Pool), nil)
	if err != nil {
		return err
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("migrate river: %w", err)
	}
	data, err := migrations.ReadFile("migrations/001_initial.sql")
	if err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, string(data)); err != nil {
		return fmt.Errorf("migrate app: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS overload_schema_migrations (version text PRIMARY KEY)`); err != nil {
		return fmt.Errorf("migrate app: %w", err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() <= "001_initial.sql" || entry.IsDir() {
			continue
		}
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(729140)`); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		var applied bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM overload_schema_migrations WHERE version=$1)`, entry.Name()).Scan(&applied)
		if err == nil && !applied {
			var migration []byte
			migration, err = migrations.ReadFile("migrations/" + entry.Name())
			if err == nil {
				_, err = tx.Exec(ctx, string(migration))
			}
			if err == nil {
				_, err = tx.Exec(ctx, `INSERT INTO overload_schema_migrations (version) VALUES ($1)`, entry.Name())
			}
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migrate %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecordDelivery(ctx context.Context, deliveryID, event, action, repoName string, payload []byte) (bool, error) {
	if deliveryID == "" || !json.Valid(payload) {
		return false, errors.New("invalid delivery")
	}
	var id int64
	err := s.Pool.QueryRow(ctx, `INSERT INTO webhook_deliveries (delivery_id, event, action, repository_full_name, payload, outcome, skip_reason) VALUES ($1, $2, $3, $4, $5, 'skipped', 'not implemented') ON CONFLICT (source, delivery_id) DO NOTHING RETURNING id`, deliveryID, event, action, repoName, payload).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) LoadRunWorkflow(ctx context.Context, runID int64) (overload.Run, overload.ResolvedWorkflow, string, error) {
	var run overload.Run
	var repoName string
	var snapshot []byte
	err := s.Pool.QueryRow(ctx, `SELECT r.id,r.repository_id,r.pr_number,r.head_sha,r.base_sha,r.status,r.dry_run,r.config_snapshot,COALESCE(r.installation_id,0),repo.full_name FROM runs r JOIN repositories repo ON repo.id=r.repository_id WHERE r.id=$1`, runID).Scan(&run.ID, &run.RepositoryID, &run.PRNumber, &run.HeadSHA, &run.BaseSHA, &run.Status, &run.DryRun, &snapshot, &run.InstallationID, &repoName)
	if err != nil {
		return run, overload.ResolvedWorkflow{}, "", err
	}
	var workflow overload.ResolvedWorkflow
	if len(snapshot) == 0 || string(snapshot) == "{}" {
		return run, workflow, repoName, errors.New("run has no pinned workflow")
	}
	if err := json.Unmarshal(snapshot, &workflow); err != nil {
		return run, workflow, repoName, err
	}
	return run, workflow, repoName, nil
}

func (s *Store) ListRuns(ctx context.Context) ([]overload.Run, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, COALESCE(repository_id,0), COALESCE(pr_number,0), head_sha, base_sha, trigger, kind, status, mode, dry_run, COALESCE(river_job_id, 0), created_at, started_at, finished_at, error_code, error_message FROM runs ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []overload.Run
	for rows.Next() {
		var run overload.Run
		if err := rows.Scan(&run.ID, &run.RepositoryID, &run.PRNumber, &run.HeadSHA, &run.BaseSHA, &run.Trigger, &run.Kind, &run.Status, &run.Mode, &run.DryRun, &run.RiverJobID, &run.CreatedAt, &run.StartedAt, &run.FinishedAt, &run.ErrorCode, &run.ErrorMessage); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

type RunEvent struct {
	At      time.Time
	Level   string
	Step    string
	Message string
}

func (s *Store) ListRunEvents(ctx context.Context, id int64) ([]RunEvent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT at, level, step, message FROM run_events WHERE run_id = $1 ORDER BY at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []RunEvent
	for rows.Next() {
		var event RunEvent
		if err := rows.Scan(&event.At, &event.Level, &event.Step, &event.Message); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

type Delivery struct {
	Event      string
	Action     string
	Repository string
	Outcome    string
	SkipReason string
	ReceivedAt time.Time
}

func (s *Store) ListDeliveries(ctx context.Context) ([]Delivery, error) {
	rows, err := s.Pool.Query(ctx, `SELECT event, action, repository_full_name, outcome, skip_reason, received_at FROM webhook_deliveries ORDER BY received_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []Delivery
	for rows.Next() {
		var delivery Delivery
		if err := rows.Scan(&delivery.Event, &delivery.Action, &delivery.Repository, &delivery.Outcome, &delivery.SkipReason, &delivery.ReceivedAt); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

func (s *Store) GetRun(ctx context.Context, id int64) (overload.Run, error) {
	var run overload.Run
	err := s.Pool.QueryRow(ctx, `SELECT id, COALESCE(repository_id,0), COALESCE(pr_number,0), head_sha, base_sha, trigger, kind, status, mode, dry_run, COALESCE(river_job_id, 0), created_at, started_at, finished_at, error_code, error_message FROM runs WHERE id = $1`, id).Scan(&run.ID, &run.RepositoryID, &run.PRNumber, &run.HeadSHA, &run.BaseSHA, &run.Trigger, &run.Kind, &run.Status, &run.Mode, &run.DryRun, &run.RiverJobID, &run.CreatedAt, &run.StartedAt, &run.FinishedAt, &run.ErrorCode, &run.ErrorMessage)
	return run, err
}

type ReviewArgs struct {
	RunID int64 `json:"run_id"`
}

func (ReviewArgs) Kind() string { return "review_pr" }
