package postgres

import (
	"context"
	"encoding/json"
	"errors"

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
	_, err = s.Pool.Exec(ctx, `INSERT INTO model_policy (id, policy) VALUES (1, $1) ON CONFLICT (id) DO UPDATE SET policy = EXCLUDED.policy, updated_at = now()`, data)
	return err
}
