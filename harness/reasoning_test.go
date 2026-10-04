package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/daltoniam/overload"
)

type capturedRequest struct {
	ReasoningEffort     string         `json:"reasoning_effort"`
	ChatTemplateKwargs  map[string]any `json:"chat_template_kwargs"`
	MaxTokens           int            `json:"max_tokens"`
	MaxCompletionTokens int            `json:"max_completion_tokens"`
}

func TestReasoningStyles(t *testing.T) {
	for _, test := range []struct {
		name          string
		model         overload.ModelProfile
		finishes      []string
		wantEfforts   []string
		wantTemplate  bool
		wantMaxTokens int
		wantLabel     string
		wantFallbacks any
	}{
		{name: "local auto keeps template xhigh", model: overload.ModelProfile{ConnectionKind: "local"}, finishes: []string{"stop"}, wantEfforts: []string{"xhigh"}, wantTemplate: true, wantMaxTokens: 49152, wantLabel: "chat_template:xhigh"},
		{name: "reasoning_effort field", model: overload.ModelProfile{ConnectionKind: "local", ReasoningParam: "reasoning_effort", ReasoningEffort: "high", MaxOutputTokens: 32768}, finishes: []string{"stop"}, wantEfforts: []string{"high"}, wantMaxTokens: 32768, wantLabel: "reasoning_effort:high"},
		{name: "field style falls back to medium", model: overload.ModelProfile{ConnectionKind: "local", ReasoningParam: "reasoning_effort", ReasoningEffort: "max"}, finishes: []string{"length", "stop"}, wantEfforts: []string{"max", "medium"}, wantMaxTokens: 49152, wantLabel: "reasoning_effort:max", wantFallbacks: 1},
		{name: "none sends nothing", model: overload.ModelProfile{ConnectionKind: "local", ReasoningParam: "none", MaxOutputTokens: 4096}, finishes: []string{"length", "stop"}, wantEfforts: []string{""}, wantMaxTokens: 4096, wantLabel: "none"},
		{name: "hosted auto unchanged", model: overload.ModelProfile{ConnectionKind: "hosted"}, finishes: []string{"stop"}, wantEfforts: []string{""}, wantMaxTokens: 16384, wantLabel: "none"},
		{name: "hosted with effort", model: overload.ModelProfile{ConnectionKind: "hosted", ReasoningParam: "reasoning_effort", ReasoningEffort: "low"}, finishes: []string{"stop"}, wantEfforts: []string{"low"}, wantMaxTokens: 16384, wantLabel: "reasoning_effort:low"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests []capturedRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request capturedRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				requests = append(requests, request)
				finish := test.finishes[min(len(requests), len(test.finishes))-1]
				content := `{"summary":"No issues","findings":[]}`
				if finish == "length" {
					content = ""
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":%q,"message":{"role":"assistant","content":%q}}]}`, finish, content)
			}))
			defer server.Close()
			test.model.Provider, test.model.BaseURL, test.model.Model = "openaicompat", server.URL, "m"
			entry := "Entry"
			workflow := overload.ResolvedWorkflow{Name: "styles", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "a", Model: test.model, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
			result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n", Workflow: workflow}, fstest.MapFS{})
			if err != nil && test.name != "none sends nothing" {
				t.Fatal(err)
			}
			for index, want := range test.wantEfforts {
				if index >= len(requests) {
					t.Fatalf("only %d requests", len(requests))
				}
				request := requests[index]
				got := request.ReasoningEffort
				if test.wantTemplate {
					got, _ = request.ChatTemplateKwargs["reasoning_effort"].(string)
				} else if request.ChatTemplateKwargs != nil {
					t.Fatalf("unexpected chat_template_kwargs %v", request.ChatTemplateKwargs)
				}
				if got != want {
					t.Fatalf("request %d effort %q, want %q", index, got, want)
				}
				if max(request.MaxTokens, request.MaxCompletionTokens) != test.wantMaxTokens {
					t.Fatalf("request %d max tokens %+v, want %d", index, request, test.wantMaxTokens)
				}
			}
			if test.name == "none sends nothing" && len(requests) != 2 {
				t.Fatalf("none style should not retry at medium; requests=%+v", requests)
			}
			if result.Metrics["agent_a_reasoning"] != test.wantLabel || result.Metrics["effort_fallbacks"] != test.wantFallbacks {
				t.Fatalf("metrics %v", result.Metrics)
			}
		})
	}
}

func TestValidateReasoning(t *testing.T) {
	for _, test := range []struct {
		param, effort string
		tokens        int
		ok            bool
	}{
		{"", "", 0, true},
		{"none", "", 1024, true},
		{"chat_template", "xhigh", 49152, true},
		{"reasoning_effort", "max", 0, true},
		{"", "high", 0, false},
		{"reasoning_effort", "", 0, false},
		{"reasoning_effort", "turbo", 0, false},
		{"thinking", "high", 0, false},
		{"none", "", 100, false},
		{"none", "", 300000, false},
	} {
		if err := overload.ValidateReasoning(test.param, test.effort, test.tokens); (err == nil) != test.ok {
			t.Errorf("%+v: %v", test, err)
		}
	}
}

func TestLocalModelsHaveNoResponseHeaderTimeout(t *testing.T) {
	transport, ok := localHTTPClient.Transport.(*http.Transport)
	if !ok || transport.ResponseHeaderTimeout != 0 || localHTTPClient.Timeout != 0 {
		t.Fatalf("local client would cut off long thinking: %+v", localHTTPClient)
	}
	if _, err := newProvider(overload.ModelProfile{ConnectionKind: "local", BaseURL: "http://127.0.0.1:1/v1"}); err != nil {
		t.Fatal(err)
	}
}

func TestModelAPIKeyEnvCannotNameOverloadSecrets(t *testing.T) {
	base := overload.ReviewSettings{Name: "m", Provider: "openaicompat", BaseURL: "https://models.example.com/v1", Model: "x", PromptProfile: "context", ConnectionKind: "hosted"}
	for name, ok := range map[string]bool{"CF_AIG_TOKEN": true, "OVERLOAD_MODEL_KEY": true, "OVERLOAD_DEFAULT_MODEL_API_KEY": true, "DATABASE_URL": false, "OVERLOAD_UI_PASSWORD": false, "GITHUB_APP_PRIVATE_KEY": false, "GH_TOKEN": false, "PGPASSWORD": false, "": false} {
		settings := base
		settings.APIKeyEnv = name
		if err := settings.Validate(); (err == nil) != ok {
			t.Errorf("api_key_env %q: %v", name, err)
		}
	}
}
