package overload

import (
	"strings"
	"testing"
)

func TestToolServerValidate(t *testing.T) {
	valid := ToolServer{Name: "switchboard", URL: "https://app.switchboard-mcp.com/orgs/acme/mcp", TokenEnv: "OVERLOAD_TOOL_SWITCHBOARD", Enabled: true}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	local := ToolServer{Name: "sb-local", URL: "http://127.0.0.1:3847/mcp"}
	if err := local.Validate(); err != nil {
		t.Fatalf("local server without token: %v", err)
	}
	cases := map[string]ToolServer{
		"bad name":            {Name: "has space", URL: valid.URL},
		"no scheme":           {Name: "a", URL: "app.switchboard-mcp.com/mcp"},
		"other scheme":        {Name: "a", URL: "ftp://example.com/mcp"},
		"credentials in URL":  {Name: "a", URL: "https://user:pass@example.com/mcp"},
		"database token":      {Name: "a", URL: valid.URL, TokenEnv: "DATABASE_URL"},
		"model key as token":  {Name: "a", URL: valid.URL, TokenEnv: "OVERLOAD_MODEL_OPENAI"},
		"bare prefix":         {Name: "a", URL: valid.URL, TokenEnv: "OVERLOAD_TOOL_"},
		"invalid token name":  {Name: "a", URL: valid.URL, TokenEnv: "OVERLOAD_TOOL_A-B"},
		"overlong describing": {Name: "a", URL: valid.URL, Description: strings.Repeat("x", 501)},
	}
	for name, server := range cases {
		if err := server.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOnlyScheduledAgentsUseTools(t *testing.T) {
	agent := AgentDefinition{Name: "researcher", Kind: "scheduled_prompt", Model: "gpt", Prompt: "Research.", Tools: []string{"switchboard"}}
	if err := agent.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*AgentDefinition){
		"pr review":       func(a *AgentDefinition) { a.Kind = "pr_review" },
		"default kind":    func(a *AgentDefinition) { a.Kind = "" },
		"repeated server": func(a *AgentDefinition) { a.Tools = []string{"sb", "sb"} },
		"bad name":        func(a *AgentDefinition) { a.Tools = []string{"a b"} },
		"too many": func(a *AgentDefinition) {
			a.Tools = nil
			for index := 0; index <= MaxAgentTools; index++ {
				a.Tools = append(a.Tools, "s"+strings.Repeat("x", index))
			}
		},
	} {
		copy := agent
		change(&copy)
		if err := copy.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRunLimits(t *testing.T) {
	scheduled := Workflow{Name: "research", Kind: "scheduled_prompt", Agents: []string{"researcher"}, Enabled: true, MaxSteps: 80, TimeoutMinutes: 60}
	if err := scheduled.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, workflow := range map[string]Workflow{
		"pr review with steps": {Name: "r", Kind: "pr_review", Agents: []string{"a"}, MaxSteps: 5},
		"too many steps":       {Name: "r", Kind: "scheduled_prompt", Agents: []string{"a"}, MaxSteps: MaxStepsLimit + 1},
		"negative timeout":     {Name: "r", Kind: "scheduled_prompt", Agents: []string{"a"}, TimeoutMinutes: -1},
		"too long":             {Name: "r", Kind: "scheduled_prompt", Agents: []string{"a"}, TimeoutMinutes: MaxTimeoutMinutes + 1},
	} {
		if err := workflow.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	steps, minutes := ResolvedWorkflow{}.RunLimits()
	if steps != DefaultMaxSteps || minutes != DefaultTimeoutMinutes {
		t.Fatalf("defaults %d %d", steps, minutes)
	}
}

func TestVerifyRejectsToolsOnPRReviews(t *testing.T) {
	entry := PromptTemplate{Kind: "entry", Body: "Review.", SHA256: PromptDigest("Review.")}
	agent := ResolvedAgent{Name: "lead", Model: ModelProfile{Provider: "openaicompat", Model: "m", BaseURL: "http://127.0.0.1:8080/v1"}, EntryPrompt: entry, Tools: []ToolServer{{Name: "sb", URL: "http://127.0.0.1:3847/mcp", Enabled: true}}}
	review := ResolvedWorkflow{Version: SnapshotVersion, Name: "r", Kind: "pr_review", Agents: []ResolvedAgent{agent}}
	if err := review.Verify(); err == nil {
		t.Fatal("PR review snapshot with tools verified")
	}
	scheduled := review
	scheduled.Kind = "scheduled_prompt"
	if err := scheduled.Verify(); err != nil {
		t.Fatalf("scheduled snapshot: %v", err)
	}
	scheduled.Agents[0].Tools[0].TokenEnv = "DATABASE_URL"
	if err := scheduled.Verify(); err == nil {
		t.Fatal("snapshot with an unsafe token variable verified")
	}
}
