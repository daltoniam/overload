package main

import (
	"context"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

type fakeConfigurationStore struct {
	prompt overload.PromptTemplate
}

func (store *fakeConfigurationStore) SavePrompt(_ context.Context, prompt overload.PromptTemplate) (overload.PromptTemplate, error) {
	if err := prompt.Validate(); err != nil {
		return prompt, err
	}
	store.prompt = prompt
	return prompt, nil
}
func (*fakeConfigurationStore) SaveAgent(context.Context, overload.AgentDefinition) error { return nil }
func (*fakeConfigurationStore) SaveWorkflow(context.Context, overload.Workflow) error     { return nil }
func (*fakeConfigurationStore) SaveBinding(context.Context, overload.TriggerBinding) error {
	return nil
}
func (*fakeConfigurationStore) SaveRepository(context.Context, string, bool, bool) error { return nil }
func (*fakeConfigurationStore) SaveSchedule(context.Context, overload.Schedule) error    { return nil }

func TestConfigurationCLIInput(t *testing.T) {
	for _, test := range []struct {
		name      string
		json      string
		wantError bool
	}{
		{"valid prompt", `{"name":"security","kind":"entry","body":"Inspect authorization."}`, false},
		{"unknown field", `{"name":"security","kind":"entry","body":"Inspect authorization.","api_key":"secret"}`, true},
		{"invalid prompt", `{"name":"security","kind":"entry","body":""}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeConfigurationStore{}
			err := applyConfiguration(context.Background(), store, "prompts", strings.NewReader(test.json))
			if (err != nil) != test.wantError || err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("unexpected error: %v", err)
			}
			if err == nil && store.prompt.Name != "security" {
				t.Fatal("prompt not saved")
			}
		})
	}
}

type previewStore struct{ workflow overload.ResolvedWorkflow }

func (store previewStore) ResolveWorkflow(context.Context, string) (overload.ResolvedWorkflow, error) {
	return store.workflow, nil
}

func TestPreviewWorkflowCLI(t *testing.T) {
	workflow := overload.ResolvedWorkflow{Version: overload.SnapshotVersion, Kind: "pr_review", SkipPaths: []string{"*.lock"}, Agents: []overload.ResolvedAgent{{Name: "lead"}}}
	var out strings.Builder
	if err := previewWorkflow(context.Background(), previewStore{workflow}, "w", strings.NewReader("a.go\ngo.lock\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"skipped":["go.lock"]`) || !strings.Contains(out.String(), `"model_calls":1`) {
		t.Fatalf("preview %s", out.String())
	}
	workflow.Kind = "scheduled_prompt"
	if err := previewWorkflow(context.Background(), previewStore{workflow}, "w", strings.NewReader("a.go"), &out); err == nil {
		t.Fatal("previewed a scheduled workflow")
	}
}
