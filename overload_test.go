package overload

import (
	"fmt"
	"testing"
)

func TestReviewSettingsValidate(t *testing.T) {
	valid := ReviewSettings{Name: "bonsai", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "bonsai-2-27b", PromptProfile: "context"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*ReviewSettings)
	}{
		{"secret URL", func(s *ReviewSettings) { s.BaseURL = "http://token:secret@example.com/v1" }},
		{"invalid scheme", func(s *ReviewSettings) { s.BaseURL = "file:///tmp/model" }},
		{"invalid prompt", func(s *ReviewSettings) { s.PromptProfile = "unsafe" }},
		{"invalid provider", func(s *ReviewSettings) { s.Provider = "native" }},
		{"invalid env", func(s *ReviewSettings) { s.APIKeyEnv = "secret-value!" }},
		{"invalid name", func(s *ReviewSettings) { s.Name = "../outside" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			setting := valid
			test.change(&setting)
			if err := setting.Validate(); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
}

func TestFindingFingerprint(t *testing.T) {
	base := Finding{Path: "a.go", Line: 10, Category: "bug", Title: "Nil  deref", Evidence: "x.y()"}
	moved := base
	moved.Line = 42
	moved.Title = "nil deref"
	if FindingFingerprint("Acme/API", 1, base) != FindingFingerprint("acme/api", 1, moved) {
		t.Fatal("line or case change altered fingerprint")
	}
	other := base
	other.Path = "b.go"
	if FindingFingerprint("acme/api", 1, base) == FindingFingerprint("acme/api", 1, other) || FindingFingerprint("acme/api", 1, base) == FindingFingerprint("acme/api", 2, base) {
		t.Fatal("distinct findings collided")
	}
}

func TestResolvedWorkflowVerifyAgentLimits(t *testing.T) {
	entry := "Review."
	agent := func(name string) ResolvedAgent {
		return ResolvedAgent{Name: name, Model: ModelProfile{Provider: "openaicompat", BaseURL: "http://127.0.0.1:8000/v1", Model: "m"}, EntryPrompt: PromptTemplate{Kind: "entry", Body: entry, SHA256: PromptDigest(entry)}}
	}
	workflow := ResolvedWorkflow{Name: "w", Kind: "pr_review"}
	for index := range 1 + MaxSubAgents {
		workflow.Agents = append(workflow.Agents, agent(fmt.Sprintf("agent-%d", index)))
	}
	if err := workflow.Verify(); err != nil {
		t.Fatalf("main agent plus %d sub-agents rejected: %v", MaxSubAgents, err)
	}
	if err := (ResolvedWorkflow{Name: "w", Kind: "pr_review", Agents: append(workflow.Agents, agent("extra"))}).Verify(); err == nil {
		t.Fatal("too many sub-agents accepted")
	}
	if err := (ResolvedWorkflow{Name: "w", Kind: "pr_review", Agents: []ResolvedAgent{agent("lead"), agent("lead")}}).Verify(); err == nil {
		t.Fatal("duplicate agent names accepted")
	}
}
