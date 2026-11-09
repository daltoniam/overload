package postgres

import (
	"context"
	"encoding/json"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// ListWorkloadRules returns the workload rules by name.
func (s *Store) ListWorkloadRules(ctx context.Context) ([]overload.WorkloadRule, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name, issuer, audience, conditions, scopes, enabled FROM workload_rules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []overload.WorkloadRule
	for rows.Next() {
		var rule overload.WorkloadRule
		var conditions []byte
		if err := rows.Scan(&rule.Name, &rule.Issuer, &rule.Audience, &conditions, &rule.Scopes, &rule.Enabled); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(conditions, &rule.Conditions); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

// SaveWorkloadRule creates or replaces a rule.
func (s *Store) SaveWorkloadRule(ctx context.Context, rule overload.WorkloadRule) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	conditions, err := json.Marshal(rule.Conditions)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO workload_rules (name, issuer, audience, conditions, scopes, enabled) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (name) DO UPDATE SET issuer = EXCLUDED.issuer, audience = EXCLUDED.audience, conditions = EXCLUDED.conditions, scopes = EXCLUDED.scopes, enabled = EXCLUDED.enabled, updated_at = now()`, rule.Name, rule.Issuer, rule.Audience, conditions, rule.Scopes, rule.Enabled)
	return err
}

// DeleteWorkloadRule deletes a rule.
func (s *Store) DeleteWorkloadRule(ctx context.Context, name string) error {
	command, err := s.Pool.Exec(ctx, `DELETE FROM workload_rules WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
