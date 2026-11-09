package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/jackc/pgx/v5"
)

// ErrModelPolicy means the model policy refuses a model.
var ErrModelPolicy = errors.New("model not allowed by the model policy")

// LoadModelPolicy returns the saved model policy, or the zero policy (which
// allows every model) when none is saved.
func (s *Store) LoadModelPolicy(ctx context.Context) (catalog.Policy, error) {
	return loadModelPolicy(ctx, s.Pool)
}

func loadModelPolicy(ctx context.Context, q querier) (catalog.Policy, error) {
	var data []byte
	err := q.QueryRow(ctx, `SELECT policy FROM model_policy WHERE id = 1`).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return catalog.Policy{}, nil
	}
	if err != nil {
		return catalog.Policy{}, err
	}
	var policy catalog.Policy
	if err := json.Unmarshal(data, &policy); err != nil {
		return catalog.Policy{}, err
	}
	return policy, nil
}

// SaveModelPolicy validates and stores the model policy. Models it refuses
// stay saved, but workflows using them cannot queue new runs.
func (s *Store) SaveModelPolicy(ctx context.Context, policy catalog.Policy) error {
	policy = policy.Normalize()
	if err := policy.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The model-save lock: a model saved under the old policy cannot commit
	// after the new policy that refuses it.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(729141)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_policy (id, policy) VALUES (1, $1) ON CONFLICT (id) DO UPDATE SET policy = EXCLUDED.policy, updated_at = now()`, data); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CheckModelPolicy checks a pinned workflow's models against the current
// policy, so a run queued before the policy changed does not start with a
// model the policy now refuses.
func (s *Store) CheckModelPolicy(ctx context.Context, workflow overload.ResolvedWorkflow) error {
	policy, err := s.LoadModelPolicy(ctx)
	if err != nil {
		return err
	}
	if err := checkWorkflowPolicy(policy, workflow); err != nil {
		return fmt.Errorf("%w: %w", ErrModelPolicy, err)
	}
	return nil
}

func checkWorkflowPolicy(policy catalog.Policy, workflow overload.ResolvedWorkflow) error {
	for _, agent := range workflow.Agents {
		if err := policy.Check(agent.Model.Catalog, agent.Model.Model).Error(); err != nil {
			return fmt.Errorf("model %s of agent %s is not allowed by the model policy: %w", agent.Model.Name, agent.Name, err)
		}
	}
	return nil
}
