package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
)

func TestModelPolicy(t *testing.T) {
	store := isolatedStore(t)
	ctx := context.Background()
	policy, err := store.LoadModelPolicy(ctx)
	if err != nil || policy.Restrictive() {
		t.Fatalf("default policy = %+v, %v", policy, err)
	}
	qwen := overload.ReviewSettings{Name: "qwen", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen3.8-flash-next", PromptProfile: "context", Catalog: "qwen3.8-flash-next"}
	if err := store.SaveReviewSettings(ctx, qwen); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetReviewSettings(ctx, "qwen")
	if err != nil || loaded.Catalog != "qwen3.8-flash-next" {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: "reviewer", Model: "qwen", Prompt: "Review the diff.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: "review", Kind: "pr_review", Agents: []string{"reviewer"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveWorkflow(ctx, "review"); err != nil {
		t.Fatalf("resolve before policy: %v", err)
	}

	if err := store.SaveModelPolicy(ctx, catalog.Policy{BlockedOrigins: []string{"cn"}}); err != nil {
		t.Fatal(err)
	}
	policy, err = store.LoadModelPolicy(ctx)
	if err != nil || strings.Join(policy.BlockedOrigins, ",") != "CN" {
		t.Fatalf("saved policy = %+v, %v", policy, err)
	}
	_, err = store.ResolveWorkflow(ctx, "review")
	if !errors.Is(err, ErrWorkflowUnavailable) || !strings.Contains(err.Error(), "model qwen of agent reviewer is not allowed") {
		t.Fatalf("resolve after policy: %v", err)
	}
	qwen.Concurrency = 2
	if err := store.SaveReviewSettings(ctx, qwen); !errors.Is(err, ErrModelPolicy) {
		t.Fatalf("saving a refused model: %v", err)
	}
	flint := overload.ReviewSettings{Name: "flint", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "flint", PromptProfile: "context", Catalog: "recursive-flint"}
	if err := store.SaveReviewSettings(ctx, flint); err != nil {
		t.Fatalf("saving an allowed model: %v", err)
	}
	if err := store.SaveModelPolicy(ctx, catalog.Policy{AllowedOrigins: []string{"US"}, BlockedOrigins: []string{"US"}}); err == nil {
		t.Fatal("saved a contradictory policy")
	}
	if err := store.SaveModelPolicy(ctx, catalog.Policy{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveWorkflow(ctx, "review"); err != nil {
		t.Fatalf("resolve after clearing policy: %v", err)
	}
}
