package postgres

import (
	"context"
	"errors"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ListReviewSettings(ctx context.Context) ([]overload.ReviewSettings, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name, provider, connection_kind, base_url, model, api_key_env, prompt_profile, is_default, concurrency, reasoning_param, reasoning_effort, max_output_tokens, api FROM model_profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var settings []overload.ReviewSettings
	for rows.Next() {
		var setting overload.ReviewSettings
		if err := rows.Scan(&setting.Name, &setting.Provider, &setting.ConnectionKind, &setting.BaseURL, &setting.Model, &setting.APIKeyEnv, &setting.PromptProfile, &setting.IsDefault, &setting.Concurrency, &setting.ReasoningParam, &setting.ReasoningEffort, &setting.MaxOutputTokens, &setting.API); err != nil {
			return nil, err
		}
		settings = append(settings, setting)
	}
	return settings, rows.Err()
}

func (s *Store) GetReviewSettings(ctx context.Context, name string) (overload.ReviewSettings, error) {
	var setting overload.ReviewSettings
	query := `SELECT name, provider, connection_kind, base_url, model, api_key_env, prompt_profile, is_default, concurrency, reasoning_param, reasoning_effort, max_output_tokens, api FROM model_profiles WHERE name=$1 OR ($1='' AND is_default=true) ORDER BY is_default DESC LIMIT 1`
	err := s.Pool.QueryRow(ctx, query, name).Scan(&setting.Name, &setting.Provider, &setting.ConnectionKind, &setting.BaseURL, &setting.Model, &setting.APIKeyEnv, &setting.PromptProfile, &setting.IsDefault, &setting.Concurrency, &setting.ReasoningParam, &setting.ReasoningEffort, &setting.MaxOutputTokens, &setting.API)
	return setting, err
}

func (s *Store) SaveReviewSettings(ctx context.Context, setting overload.ReviewSettings) error {
	if err := setting.Validate(); err != nil {
		return err
	}
	if setting.ConnectionKind == "" {
		setting.ConnectionKind = "local"
	}
	if setting.Concurrency == 0 {
		setting.Concurrency = 1
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(729141)`); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM model_profiles`).Scan(&count); err != nil {
		return err
	}
	if setting.IsDefault || count == 0 {
		setting.IsDefault = true
		if _, err := tx.Exec(ctx, `UPDATE model_profiles SET is_default=false WHERE is_default=true`); err != nil {
			return err
		}
	} else {
		var current bool
		err := tx.QueryRow(ctx, `SELECT is_default FROM model_profiles WHERE name=$1`, setting.Name).Scan(&current)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		setting.IsDefault = err == nil && current
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_profiles (name, provider, connection_kind, base_url, model, api_key_env, prompt_profile, is_default, concurrency, reasoning_param, reasoning_effort, max_output_tokens, api) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) ON CONFLICT (name) DO UPDATE SET provider=EXCLUDED.provider, connection_kind=EXCLUDED.connection_kind, base_url=EXCLUDED.base_url, model=EXCLUDED.model, api_key_env=EXCLUDED.api_key_env, prompt_profile=EXCLUDED.prompt_profile, is_default=EXCLUDED.is_default, concurrency=EXCLUDED.concurrency, reasoning_param=EXCLUDED.reasoning_param, reasoning_effort=EXCLUDED.reasoning_effort, max_output_tokens=EXCLUDED.max_output_tokens, api=EXCLUDED.api, updated_at=now()`, setting.Name, setting.Provider, setting.ConnectionKind, setting.BaseURL, setting.Model, setting.APIKeyEnv, setting.PromptProfile, setting.IsDefault, setting.Concurrency, setting.ReasoningParam, setting.ReasoningEffort, setting.MaxOutputTokens, setting.API)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteReviewSettings(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("profile name required")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(729141)`); err != nil {
		return err
	}
	var isDefault bool
	if err := tx.QueryRow(ctx, `SELECT is_default FROM model_profiles WHERE name=$1`, name).Scan(&isDefault); err != nil {
		return err
	}
	if isDefault {
		return errors.New("select another default before deleting this profile")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
