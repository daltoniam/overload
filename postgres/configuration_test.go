package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/daltoniam/overload"
)

func TestWorkflowConfiguration(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM trigger_bindings WHERE repository_full_name='test/workflow-configuration'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM workflows WHERE name='test-workflow-configuration'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM agent_definitions WHERE name='test-agent-configuration'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name IN ('test-entry-configuration','test-review-configuration'))`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM prompt_templates WHERE name IN ('test-entry-configuration','test-review-configuration')`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE name='test-model-configuration'`)
		store.Pool.Close()
	})
	model := overload.ReviewSettings{Name: "test-model-configuration", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "test", PromptProfile: "context"}
	if err := store.SaveReviewSettings(ctx, model); err != nil {
		t.Fatal(err)
	}
	entry, err := store.SavePrompt(ctx, overload.PromptTemplate{Name: "test-entry-configuration", Kind: "entry", Body: "Review safely."})
	if err != nil || entry.Revision < 1 || entry.SHA256 == "" {
		t.Fatalf("entry prompt: %+v %v", entry, err)
	}
	review, err := store.SavePrompt(ctx, overload.PromptTemplate{Name: "test-review-configuration", Kind: "review", Body: "Check status handling."})
	if err != nil || review.Revision < 1 {
		t.Fatalf("review prompt: %+v %v", review, err)
	}
	agent := overload.AgentDefinition{Name: "test-agent-configuration", Model: model.Name, EntryPrompt: entry.Name, ReviewPrompt: review.Name, Enabled: true}
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	storedAgents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, stored := range storedAgents {
		if stored.Name == agent.Name && stored.Kind == "pr_review" && stored.ReviewPrompt == agent.ReviewPrompt {
			found = true
		}
	}
	if !found {
		t.Fatalf("PR agent kind or pass was not saved: %+v", storedAgents)
	}
	workflow := overload.Workflow{Name: "test-workflow-configuration", Kind: "pr_review", Agents: []string{agent.Name}, Enabled: true}
	if err := store.SaveWorkflow(ctx, workflow); err != nil {
		t.Fatal(err)
	}
	binding := overload.TriggerBinding{Source: "github", Event: "pull_request", Action: "opened", Repository: "test/workflow-configuration", Workflow: workflow.Name, Enabled: true}
	if err := store.SaveBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListBindings(ctx)
	bindingSaved := false
	for _, saved := range bindings {
		bindingSaved = bindingSaved || (saved.Repository == binding.Repository && saved.Workflow == workflow.Name)
	}
	if err != nil || !bindingSaved {
		t.Fatalf("binding not saved: %+v %v", bindings, err)
	}
	resolved, err := store.ResolveWorkflow(ctx, workflow.Name)
	if err != nil || len(resolved.Agents) != 1 || resolved.Agents[0].EntryPrompt.Body != entry.Body || resolved.Agents[0].ReviewPrompt.Body != review.Body || resolved.Agents[0].Model.ConnectionKind != "local" {
		t.Fatalf("workflow: %+v %v", resolved, err)
	}
	updated, err := store.SavePrompt(ctx, overload.PromptTemplate{Name: entry.Name, Kind: "entry", Body: "Changed prompt."})
	if err != nil || updated.Revision != entry.Revision+1 {
		t.Fatalf("revised prompt: %+v %v", updated, err)
	}
	resolvedAgain, err := store.ResolveWorkflow(ctx, workflow.Name)
	if err != nil || resolvedAgain.Agents[0].EntryPrompt.SHA256 != entry.SHA256 {
		t.Fatalf("pinned prompt changed: %+v %v", resolvedAgain, err)
	}
	model.ConnectionKind, model.APIKeyEnv = "hosted", "TEST_MODEL_KEY"
	model.Concurrency = 4
	model.ReasoningParam, model.ReasoningEffort, model.MaxOutputTokens = "reasoning_effort", "max", 65536
	if err := store.SaveReviewSettings(ctx, model); err != nil {
		t.Fatal(err)
	}
	hosted, err := store.ResolveWorkflow(ctx, workflow.Name)
	if err != nil || hosted.Agents[0].Model.ConnectionKind != "hosted" || hosted.Agents[0].Model.Model != "test" || hosted.Agents[0].Model.Concurrency != 4 || hosted.Agents[0].Model.ReasoningEffort != "max" || hosted.Agents[0].Model.MaxOutputTokens != 65536 {
		t.Fatalf("hosted model not resolved: %+v %v", hosted, err)
	}
}
