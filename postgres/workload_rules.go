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

// CreateWorkloadRule adds a rule. It fails with ErrWorkloadRuleExists
// rather than replacing a rule of the same name.
func (s *Store) CreateWorkloadRule(ctx context.Context, rule overload.WorkloadRule) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	conditions, err := json.Marshal(rule.Conditions)
	if err != nil {
		return err
	}
	command, err := s.Pool.Exec(ctx, `INSERT INTO workload_rules (name, issuer, audience, conditions, scopes, enabled) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (name) DO NOTHING`, rule.Name, rule.Issuer, rule.Audience, conditions, rule.Scopes, rule.Enabled)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return overload.ErrWorkloadRuleExists
	}
	return nil
}

// UpdateWorkloadRule replaces the rule with rule's name; pgx.ErrNoRows
// means there is none.
func (s *Store) UpdateWorkloadRule(ctx context.Context, rule overload.WorkloadRule) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	conditions, err := json.Marshal(rule.Conditions)
	if err != nil {
		return err
	}
	command, err := s.Pool.Exec(ctx, `UPDATE workload_rules SET issuer = $2, audience = $3, conditions = $4, scopes = $5, enabled = $6, updated_at = now() WHERE name = $1`, rule.Name, rule.Issuer, rule.Audience, conditions, rule.Scopes, rule.Enabled)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
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
