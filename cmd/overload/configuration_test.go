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
