package postgres

import (
	"context"
	"errors"
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
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM agent_definitions WHERE name IN ('test-agent-configuration','test-agent-configuration-security')`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name IN ('agent:test-agent-configuration','agent:test-agent-configuration-security','workflow:test-workflow-configuration'))`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM prompt_templates WHERE name IN ('agent:test-agent-configuration','agent:test-agent-configuration-security','workflow:test-workflow-configuration')`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE name='test-model-configuration'`)
		store.Pool.Close()
	})
	model := overload.ReviewSettings{Name: "test-model-configuration", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "test", PromptProfile: "context"}
	if err := store.SaveReviewSettings(ctx, model); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: "test-agent-configuration", Model: "missing-model", Prompt: "Review safely.", Enabled: true}); err == nil {
		t.Fatal("saved an agent with an unknown model")
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: "test-agent-configuration", Model: model.Name, Prompt: "  ", Enabled: true}); err == nil {
		t.Fatal("saved an agent without a prompt")
	}
	agent := overload.AgentDefinition{Name: "test-agent-configuration", Model: model.Name, Prompt: "Review safely.", Enabled: true}
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	stored := findAgent(t, store, agent.Name)
	if stored.Kind != "pr_review" || stored.Prompt != agent.Prompt || stored.PromptRevision != 1 {
		t.Fatalf("agent not saved, or saving unchanged text added a revision: %+v", stored)
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
	if err != nil || len(resolved.Agents) != 1 || resolved.Agents[0].EntryPrompt.Body != agent.Prompt || resolved.Agents[0].EntryPrompt.Revision != 1 || resolved.Agents[0].LegacyFocus.Kind != "" || resolved.Agents[0].Model.ConnectionKind != "local" || resolved.Verify() != nil {
		t.Fatalf("workflow: %+v %v", resolved, err)
	}
	if resolved.Version != overload.SnapshotVersion || resolved.Agents[0].Scope.Paths != nil || resolved.SkipPaths != nil {
		t.Fatalf("unrouted workflow snapshot: %+v", resolved)
	}
	security := agent
	security.Name = "test-agent-configuration-security"
	if err := store.SaveAgent(ctx, security); err != nil {
		t.Fatal(err)
	}
	routed := workflow
	routed.Agents = []string{agent.Name, security.Name}
	routed.Scopes = map[string]overload.Scope{security.Name: {Paths: []string{"**/auth/**"}, MaxFindings: 3}}
	routed.SkipPaths, routed.MainReviews, routed.MaxFileReviews = []string{"*.lock"}, overload.MainReviewsUnclaimed, 40
	if err := store.SaveWorkflow(ctx, routed); err != nil {
		t.Fatal(err)
	}
	stored2 := findWorkflow(t, store, routed.Name)
	if stored2.Scopes[security.Name].MaxFindings != 3 || stored2.MainReviews != overload.MainReviewsUnclaimed || stored2.MaxFileReviews != 40 || len(stored2.SkipPaths) != 1 || stored2.PlannerPrompt != "" {
		t.Fatalf("routing not listed: %+v", stored2)
	}
	resolvedRouted, err := store.ResolveWorkflow(ctx, routed.Name)
	if err != nil || resolvedRouted.Verify() != nil || resolvedRouted.Agents[1].Scope.Paths[0] != "**/auth/**" || resolvedRouted.Agents[0].Scope.Paths != nil || resolvedRouted.SkipPaths[0] != "*.lock" || resolvedRouted.MaxFileReviews != 40 {
		t.Fatalf("routing not resolved: %+v %v", resolvedRouted, err)
	}
	if resolvedRouted.PlannerPrompt != nil || resolvedRouted.VerifierPrompt != nil {
		t.Fatalf("unexpected planner: %+v", resolvedRouted)
	}

	planned := routed
	planned.PlannerPrompt, planned.VerifierPrompt = "Plan v1.", "Verify v1."
	if err := store.SaveWorkflow(ctx, planned); err != nil {
		t.Fatal(err)
	}
	pinned, err := store.ResolveWorkflow(ctx, planned.Name)
	if err != nil {
		t.Fatal(err)
	}
	planned.PlannerPrompt = "Plan v2."
	if err := store.SaveWorkflow(ctx, planned); err != nil {
		t.Fatal(err)
	}
	if listed := findWorkflow(t, store, planned.Name); listed.PlannerPrompt != "Plan v2." || listed.VerifierPrompt != "Verify v1." {
		t.Fatalf("planner text not listed: %+v", listed)
	}
	resolvedPlanned, err := store.ResolveWorkflow(ctx, planned.Name)
	if err != nil || resolvedPlanned.Verify() != nil || resolvedPlanned.PlannerPrompt.Body != "Plan v2." || resolvedPlanned.PlannerPrompt.Revision != 2 || resolvedPlanned.VerifierPrompt.Body != "Verify v1." || resolvedPlanned.VerifierPrompt.Revision != 1 {
		t.Fatalf("planner revisions: %+v %v", resolvedPlanned, err)
	}
	if pinned.PlannerPrompt.Body != "Plan v1." || pinned.Verify() != nil {
		t.Fatalf("an earlier snapshot must keep its planner text: %+v", pinned.PlannerPrompt)
	}
	if err := store.SaveWorkflow(ctx, workflow); err != nil {
		t.Fatal(err)
	}
	if listed := findWorkflow(t, store, workflow.Name); listed.PlannerPrompt != "" || listed.VerifierPrompt != "" {
		t.Fatalf("cleared planner still listed: %+v", listed)
	}

	opened := findWorkflow(t, store, workflow.Name)
	if err := store.SaveWorkflow(ctx, opened); err != nil {
		t.Fatal(err)
	}
	if again := findWorkflow(t, store, workflow.Name); again.Revision != opened.Revision {
		t.Fatalf("saving an unchanged workflow bumped its revision: %d -> %d", opened.Revision, again.Revision)
	}
	first, second := opened, opened
	first.Enabled, second.MaxFileReviews = false, 7
	if err := store.SaveWorkflow(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("a save from an older copy overwrote a newer one: %v", err)
	}
	if latest := findWorkflow(t, store, workflow.Name); latest.Enabled || latest.MaxFileReviews == 7 || latest.Revision != opened.Revision+1 {
		t.Fatalf("workflow after conflict: %+v", latest)
	}
	workflow.Enabled = true
	if err := store.SaveWorkflow(ctx, workflow); err != nil {
		t.Fatalf("a save without a revision must not be checked: %v", err)
	}

	staleAgent := findAgent(t, store, agent.Name)
	agent.Prompt, agent.PromptRevision = "Edited in another tab.", staleAgent.PromptRevision
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	staleAgent.Prompt = "Edited from an older copy."
	if err := store.SaveAgent(ctx, staleAgent); !errors.Is(err, ErrConflict) {
		t.Fatalf("an agent edit from an older copy overwrote newer instructions: %v", err)
	}
	agent.PromptRevision = 0
	agent.Prompt = "Changed prompt."
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if changed := findAgent(t, store, agent.Name); changed.Prompt != agent.Prompt || changed.PromptRevision != 3 {
		t.Fatalf("changed prompt not saved as revision 3: %+v", changed)
	}
	if security := findAgent(t, store, security.Name); security.Prompt != "Review safely." {
		t.Fatalf("agents must not share prompts: %+v", security)
	}
	if resolved.Agents[0].EntryPrompt.Body != "Review safely." || resolved.Verify() != nil {
		t.Fatalf("an earlier snapshot must keep its prompt: %+v", resolved.Agents[0].EntryPrompt)
	}
	resolvedAgain, err := store.ResolveWorkflow(ctx, workflow.Name)
	if err != nil || resolvedAgain.Agents[0].EntryPrompt.Body != agent.Prompt || resolvedAgain.Agents[0].EntryPrompt.Revision != 3 {
		t.Fatalf("new runs must use the changed prompt: %+v %v", resolvedAgain, err)
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

func findAgent(t *testing.T, store *Store, name string) overload.AgentDefinition {
	t.Helper()
	agents, err := store.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if agent.Name == name {
			return agent
		}
	}
	t.Fatalf("agent %s not listed", name)
	return overload.AgentDefinition{}
}

func findWorkflow(t *testing.T, store *Store, name string) overload.Workflow {
	t.Helper()
	workflows, err := store.ListWorkflows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, workflow := range workflows {
		if workflow.Name == name {
			return workflow
		}
	}
	t.Fatalf("workflow %s not listed", name)
	return overload.Workflow{}
}
