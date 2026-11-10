package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres/pgtest"
	"github.com/jackc/pgx/v5"
)

func TestWorkloadRules(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	rule := overload.WorkloadRule{Name: "ci", Issuer: overload.GitHubActionsIssuer, Audience: "https://overload.example.com", Conditions: map[string]string{"repository": "acme/*", "ref": "refs/heads/main"}, Scopes: []string{overload.ScopeReviews}, Enabled: true}
	if err := store.UpdateWorkloadRule(ctx, rule); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("updated a missing rule: %v", err)
	}
	if err := store.CreateWorkloadRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	rule.Scopes = []string{overload.ScopeReviews, overload.ScopeModels}
	if err := store.CreateWorkloadRule(ctx, rule); !errors.Is(err, overload.ErrWorkloadRuleExists) {
		t.Fatalf("created a second rule named ci: %v", err)
	}
	if err := store.UpdateWorkloadRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkloadRule(ctx, overload.WorkloadRule{Name: "bad", Issuer: overload.GitHubActionsIssuer}); err == nil {
		t.Fatal("saved an invalid rule")
	}
	rules, err := store.ListWorkloadRules(ctx)
	if err != nil || len(rules) != 1 || len(rules[0].Scopes) != 2 || rules[0].Conditions["ref"] != "refs/heads/main" {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	if err := store.DeleteWorkloadRule(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWorkloadRule(ctx, "ci"); err == nil {
		t.Fatal("deleted a missing rule")
	}
}
