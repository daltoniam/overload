package main

import (
	"context"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

type fakeConfigurationStore struct {
	agent overload.AgentDefinition
}

func (store *fakeConfigurationStore) SaveAgent(_ context.Context, agent overload.AgentDefinition) error {
	if err := agent.Validate(); err != nil {
		return err
	}
	store.agent = agent
	return nil
}
func (*fakeConfigurationStore) SaveWorkflow(context.Context, overload.Workflow) error { return nil }
func (*fakeConfigurationStore) SaveBinding(context.Context, overload.TriggerBinding) error {
	return nil
}
func (*fakeConfigurationStore) SaveRepository(context.Context, string, bool, bool) error { return nil }
func (*fakeConfigurationStore) SaveSchedule(context.Context, overload.Schedule) error    { return nil }

func TestConfigurationCLIInput(t *testing.T) {
	for _, test := range []struct {
		name      string
		json      string
		wantError string
	}{
		{"valid agent", `{"name":"security","model":"qwen","prompt":"Inspect authorization.","enabled":true}`, ""},
		{"unknown field", `{"name":"security","model":"qwen","prompt":"Inspect authorization.","api_key":"secret"}`, "unknown field"},
		{"missing prompt", `{"name":"security","model":"qwen","prompt":""}`, "prompt is required"},
		{"old prompt reference", `{"name":"security","model":"qwen","entry_prompt":"base"}`, `hold their prompt text in "prompt"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeConfigurationStore{}
			err := applyConfiguration(context.Background(), store, "agents", strings.NewReader(test.json))
			if test.wantError == "" && err != nil || test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) || err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("unexpected error: %v", err)
			}
			if err == nil && store.agent.Prompt != "Inspect authorization." {
				t.Fatal("agent not saved")
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

func TestConfigurationCLIRejectsOversizedOrExtraInput(t *testing.T) {
	store := &fakeConfigurationStore{}
	huge := `{"name":"a","model":"m","prompt":"` + strings.Repeat("x", maxConfigurationInput) + `"}`
	if err := applyConfiguration(context.Background(), store, "agents", strings.NewReader(huge)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized input: %v", err)
	}
	two := `{"name":"a","model":"m","prompt":"p"} {"name":"b","model":"m","prompt":"p"}`
	if err := applyConfiguration(context.Background(), store, "agents", strings.NewReader(two)); err == nil || !strings.Contains(err.Error(), "exactly one object") {
		t.Fatalf("two objects: %v", err)
	}
	if err := applyConfiguration(context.Background(), store, "workflows", strings.NewReader(`{"name":"w","passes":[]}`)); err == nil || !strings.Contains(err.Error(), `unknown field "passes"`) {
		t.Fatalf("unknown workflow field should be named: %v", err)
	}
}
