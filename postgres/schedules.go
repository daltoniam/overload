package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/robfig/cron/v3"
)

func (s *Store) SaveSchedule(ctx context.Context, schedule overload.Schedule) error {
	if schedule.Name == "" || len(schedule.Name) > 64 || len(schedule.Input) > 4096 || len(schedule.Input) > 0 && !json.Valid(schedule.Input) {
		return errors.New("invalid schedule")
	}
	parsed, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(schedule.Cron)
	if err != nil {
		return errors.New("invalid cron expression")
	}
	zone, err := time.LoadLocation(schedule.Timezone)
	if err != nil || schedule.Timezone == "" {
		return errors.New("invalid schedule timezone")
	}
	next := parsed.Next(time.Now().In(zone))
	if next.IsZero() {
		return errors.New("schedule has no future occurrence")
	}
	if len(schedule.Input) == 0 {
		schedule.Input = json.RawMessage(`{}`)
	}
	var input map[string]any
	if err := json.Unmarshal(schedule.Input, &input); err != nil || input == nil {
		return errors.New("schedule input must be a JSON object")
	}
	var kind string
	if err := s.Pool.QueryRow(ctx, `SELECT kind FROM workflows WHERE name=$1 AND enabled`, schedule.Workflow).Scan(&kind); err != nil {
		return err
	}
	if kind != "scheduled_prompt" {
		return errors.New("workflow is not a scheduled prompt")
	}
	resolved, err := s.ResolveWorkflow(ctx, schedule.Workflow)
	if err != nil {
		return err
	}
	if err := resolved.Verify(); err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO schedules(name,workflow_id,cron,timezone,input_json,next_run_at,enabled) VALUES ($1,(SELECT id FROM workflows WHERE name=$2),$3,$4,$5,$6,$7) ON CONFLICT(name) DO UPDATE SET workflow_id=EXCLUDED.workflow_id,cron=EXCLUDED.cron,timezone=EXCLUDED.timezone,input_json=EXCLUDED.input_json,next_run_at=EXCLUDED.next_run_at,enabled=EXCLUDED.enabled,updated_at=now()`, schedule.Name, schedule.Workflow, schedule.Cron, schedule.Timezone, schedule.Input, next, schedule.Enabled)
	return err
}

func (s *Store) ListSchedules(ctx context.Context) ([]overload.Schedule, error) {
	rows, err := s.Pool.Query(ctx, `SELECT s.name,w.name,s.cron,s.timezone,s.input_json,s.next_run_at,s.enabled FROM schedules s JOIN workflows w ON w.id=s.workflow_id ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []overload.Schedule
	for rows.Next() {
		var schedule overload.Schedule
		if err := rows.Scan(&schedule.Name, &schedule.Workflow, &schedule.Cron, &schedule.Timezone, &schedule.Input, &schedule.NextRunAt, &schedule.Enabled); err != nil {
			return nil, err
		}
		schedules = append(schedules, schedule)
	}
	return schedules, rows.Err()
}

func (s *Store) EnqueueDueSchedules(ctx context.Context, client *river.Client[pgx.Tx]) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT s.id,s.name,w.name,s.cron,s.timezone,s.input_json,s.next_run_at FROM schedules s JOIN workflows w ON w.id=s.workflow_id WHERE s.enabled AND w.enabled AND s.next_run_at<=now() ORDER BY s.next_run_at LIMIT 10 FOR UPDATE OF s SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	var due []struct {
		id                                   int64
		name, workflow, expression, timezone string
		input                                []byte
		at                                   time.Time
	}
	for rows.Next() {
		var item struct {
			id                                   int64
			name, workflow, expression, timezone string
			input                                []byte
			at                                   time.Time
		}
		if err := rows.Scan(&item.id, &item.name, &item.workflow, &item.expression, &item.timezone, &item.input, &item.at); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, item := range due {
		zone, err := time.LoadLocation(item.timezone)
		if err != nil {
			return 0, err
		}
		parsed, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(item.expression)
		if err != nil {
			return 0, err
		}
		next := parsed.Next(time.Now().In(zone))
		if next.IsZero() {
			return 0, errors.New("schedule has no future occurrence")
		}
		resolved, err := s.ResolveWorkflow(ctx, item.workflow)
		if err != nil {
			return 0, err
		}
		if resolved.Kind != "scheduled_prompt" {
			return 0, errors.New("schedule workflow kind changed")
		}
		snapshot, err := json.Marshal(resolved)
		if err != nil {
			return 0, err
		}
		var runID int64
		if err := tx.QueryRow(ctx, `INSERT INTO runs(kind,pr_number,trigger,status,dry_run,config_snapshot,input_snapshot,schedule_id,scheduled_for) VALUES ('scheduled_prompt',NULL,'cron','queued',true,$1,$2,$3,$4) RETURNING id`, snapshot, item.input, item.id, item.at).Scan(&runID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schedule_occurrences(schedule_id,scheduled_for,run_id) VALUES ($1,$2,$3)`, item.id, item.at, runID); err != nil {
			return 0, err
		}
		job, err := client.InsertTx(ctx, tx, ScheduleArgs{RunID: runID}, nil)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET river_job_id=$2 WHERE id=$1`, runID, job.Job.ID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE schedules SET next_run_at=$2 WHERE id=$1`, item.id, next); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(due), nil
}

type ScheduleArgs struct {
	RunID int64 `json:"run_id"`
}

func (ScheduleArgs) Kind() string { return "scheduled_prompt" }

// RunScheduleNow queues one run of a schedule's workflow with the
// schedule's input, outside its cron times. It returns the run's ID.
func (s *Store) RunScheduleNow(ctx context.Context, client *river.Client[pgx.Tx], name string) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var scheduleID int64
	var workflowName string
	var input []byte
	err = tx.QueryRow(ctx, `SELECT s.id,w.name,s.input_json FROM schedules s JOIN workflows w ON w.id=s.workflow_id WHERE s.name=$1 FOR SHARE OF s`, name).Scan(&scheduleID, &workflowName, &input)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("schedule %q not found", name)
	}
	if err != nil {
		return 0, err
	}
	resolved, err := resolveWorkflow(ctx, tx, workflowName)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: workflow %s is disabled or missing", ErrWorkflowUnavailable, workflowName)
	}
	if err != nil {
		return 0, err
	}
	if resolved.Kind != "scheduled_prompt" {
		return 0, errors.New("schedule workflow is not a scheduled job")
	}
	if err := resolved.Verify(); err != nil {
		return 0, err
	}
	snapshot, err := json.Marshal(resolved)
	if err != nil {
		return 0, err
	}
	var runID int64
	if err := tx.QueryRow(ctx, `INSERT INTO runs(kind,pr_number,trigger,status,dry_run,config_snapshot,input_snapshot,schedule_id,scheduled_for) VALUES ('scheduled_prompt',NULL,'manual','queued',true,$1,$2,$3,now()) RETURNING id`, snapshot, input, scheduleID).Scan(&runID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'info', 'queued', 'Queued by hand')`, runID); err != nil {
		return 0, err
	}
	job, err := client.InsertTx(ctx, tx, ScheduleArgs{RunID: runID}, nil)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET river_job_id=$2 WHERE id=$1`, runID, job.Job.ID); err != nil {
		return 0, err
	}
	return runID, tx.Commit(ctx)
}
