package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Store) DeletePrompt(ctx context.Context, kind, name string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `SELECT id FROM prompt_templates WHERE kind=$1 AND name=$2 FOR UPDATE`, kind, name).Scan(&id); err != nil {
		return err
	}
	var inUse bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_definitions a JOIN prompt_revisions r ON r.id = a.entry_prompt_revision_id WHERE r.template_id=$1)`, id).Scan(&inUse); err != nil {
		return err
	}
	if inUse {
		return errors.New("prompt is used by an agent")
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflows w JOIN prompt_revisions r ON r.id::text IN (w.routing->>'planner_prompt_revision_id', w.routing->>'verifier_prompt_revision_id') WHERE r.template_id=$1)`, id).Scan(&inUse); err != nil {
		return err
	}
	if inUse {
		return errors.New("prompt is used by a workflow")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM prompt_templates WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteWorkflow(ctx context.Context, name string) error {
	command, err := s.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1 AND NOT EXISTS(SELECT 1 FROM trigger_bindings WHERE workflow_id=workflows.id) AND NOT EXISTS(SELECT 1 FROM schedules WHERE workflow_id=workflows.id)`, name)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return errors.New("workflow is used by a binding or schedule, or is unavailable")
	}
	return nil
}

func (s *Store) DeleteSchedule(ctx context.Context, name string) error {
	command, err := s.Pool.Exec(ctx, `DELETE FROM schedules WHERE name=$1 AND NOT EXISTS(SELECT 1 FROM runs WHERE schedule_id=schedules.id) AND NOT EXISTS(SELECT 1 FROM schedule_occurrences WHERE schedule_id=schedules.id)`, name)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return errors.New("schedule has run history or is unavailable")
	}
	return nil
}

func (s *Store) DeleteAgent(ctx context.Context, name string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `SELECT id FROM agent_definitions WHERE name=$1 FOR UPDATE`, name).Scan(&id); err != nil {
		return err
	}
	var inUse bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflows WHERE agent_names ? $1)`, name).Scan(&inUse); err != nil {
		return err
	}
	if inUse {
		return errors.New("agent is used by a workflow")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_definitions WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteRepository(ctx context.Context, id int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var name string
	if err := tx.QueryRow(ctx, `SELECT full_name FROM repositories WHERE id=$1 FOR UPDATE`, id).Scan(&name); err != nil {
		return err
	}
	var inUse bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE repository_id=$1) OR EXISTS(SELECT 1 FROM trigger_bindings WHERE repository_full_name=$2)`, id, name).Scan(&inUse); err != nil {
		return err
	}
	if inUse {
		return errors.New("repository has runs or event bindings; disable it instead")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM repositories WHERE id=$1`, id); err != nil {
		return fmt.Errorf("delete repository: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteBinding(ctx context.Context, id int64, repository string) error {
	command, err := s.Pool.Exec(ctx, `DELETE FROM trigger_bindings WHERE id=$1 AND repository_full_name=$2 AND NOT EXISTS (SELECT 1 FROM runs WHERE binding_id=$1)`, id, repository)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
