package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

type fakeStarterStore struct {
	agents    []overload.AgentDefinition
	workflows []overload.Workflow
}

func (store *fakeStarterStore) ListReviewSettings(context.Context) ([]overload.ReviewSettings, error) {
	return []overload.ReviewSettings{{Name: "flint"}}, nil
}
func (store *fakeStarterStore) SaveAgent(_ context.Context, agent overload.AgentDefinition) error {
	if err := agent.Validate(); err != nil {
		return err
	}
	store.agents = append(store.agents, agent)
	return nil
}
func (store *fakeStarterStore) SaveWorkflow(_ context.Context, workflow overload.Workflow) error {
	store.workflows = append(store.workflows, workflow)
	return nil
}

func TestStarterCohort(t *testing.T) {
	store := &fakeStarterStore{}
	var out bytes.Buffer
	if err := createStarterCohort(context.Background(), store, []string{"--model", "missing"}, &out); err == nil {
		t.Fatal("accepted an unknown model")
	}
	if err := createStarterCohort(context.Background(), store, []string{"--model", "flint", "--name", "team", "--max-review-tokens", "500000"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(store.agents) != 5 || len(store.workflows) != 1 {
		t.Fatalf("agents %d workflows %d", len(store.agents), len(store.workflows))
	}
	workflow := store.workflows[0]
	if workflow.Agents[0] != "team-main" || workflow.VerifierPrompt == "" || workflow.MaxReviewTokens != 500000 || len(workflow.SensitivePaths) == 0 || len(workflow.Scopes["team-performance"].Tiers) != 1 || workflow.Validate() != nil {
		t.Fatalf("workflow = %+v", workflow)
	}
	if !strings.Contains(out.String(), "overload review --checkout") {
		t.Fatalf("output = %s", out.String())
	}
}
