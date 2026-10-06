package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/daltoniam/overload"
)

func TestHostedReviewUsesCompletionTokenParameter(t *testing.T) {
	t.Setenv("OVERLOAD_HOSTED_TEST_KEY", "hosted-test-token")
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if _, ok := request["max_tokens"]; ok || string(request["max_completion_tokens"]) != "16384" {
			t.Errorf("hosted token parameters: max_tokens present=%v max_completion_tokens=%s", ok, request["max_completion_tokens"])
		}
		if _, ok := request["chat_template_kwargs"]; ok {
			t.Errorf("hosted request must not carry local reasoning effort")
		}
		if !strings.Contains(string(request["messages"]), "Hosted entry") {
			t.Errorf("missing hosted system prompt")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer hosted-test-token" {
			t.Errorf("missing environment-derived credential")
		}
		content := `{"summary":"No issues","findings":[]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"gpt-6-sol","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, content)
	}))
	defer server.Close()
	entry := "Hosted entry"
	workflow := overload.ResolvedWorkflow{Name: "hosted-test", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "hosted", Model: overload.ModelProfile{Provider: "openaicompat", ConnectionKind: "hosted", BaseURL: server.URL, Model: "gpt-6-sol", APIKeyEnv: "OVERLOAD_HOSTED_TEST_KEY"}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
	spec := overload.ReviewSpec{Diff: "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n", Workflow: workflow}
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
	if err != nil || !called || len(result.Findings) != 0 {
		t.Fatalf("called=%v result=%+v err=%v", called, result, err)
	}
}

func TestLocalReviewRequestsXHighReasoningEffort(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	entry := "Local entry"
	for _, test := range []struct {
		name string
		spec func(url string) overload.ReviewSpec
	}{
		{"workflow", func(url string) overload.ReviewSpec {
			return overload.ReviewSpec{Diff: diff, Workflow: overload.ResolvedWorkflow{Name: "local-test", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "local", Model: overload.ModelProfile{Provider: "openaicompat", ConnectionKind: "local", BaseURL: url, Model: "bonsai-2-27b"}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}}
		}},
		{"saved model", func(url string) overload.ReviewSpec {
			return profileSpec(t, diff, overload.ModelProfile{BaseURL: url, Model: "bonsai-2-27b"}, "context")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct {
					MaxTokens          int            `json:"max_tokens"`
					ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.ChatTemplateKwargs["reasoning_effort"] != "xhigh" || request.MaxTokens != localMaxOutputTokens {
					t.Errorf("local request effort=%v max_tokens=%d", request.ChatTemplateKwargs["reasoning_effort"], request.MaxTokens)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"bonsai-2-27b","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, `{"summary":"No issues","findings":[]}`)
			}))
			defer server.Close()
			if _, err := (Reviewer{}).Review(context.Background(), test.spec(server.URL), fstest.MapFS{}); err != nil || calls == 0 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestLocalReviewFallsBackToMediumWhenThinkingExhaustsBudget(t *testing.T) {
	var efforts []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		efforts = append(efforts, request.ChatTemplateKwargs["reasoning_effort"])
		w.Header().Set("Content-Type", "application/json")
		if len(efforts) == 1 {
			_, _ = fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"","reasoning_content":"still thinking"}}]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, `{"summary":"No issues","findings":[]}`)
	}))
	defer server.Close()
	entry := "Local entry"
	workflow := overload.ResolvedWorkflow{Name: "fallback", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "local", Model: overload.ModelProfile{Provider: "openaicompat", ConnectionKind: "local", BaseURL: server.URL, Model: "m"}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
	spec := overload.ReviewSpec{Diff: "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n", Workflow: workflow}
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	if len(efforts) != 2 || efforts[0] != "xhigh" || efforts[1] != "medium" || result.Metrics["effort_fallbacks"] != 1 {
		t.Fatalf("efforts=%v metrics=%v", efforts, result.Metrics)
	}
}

func profileSpec(t *testing.T, diff string, profile overload.ModelProfile, prompt string) overload.ReviewSpec {
	t.Helper()
	profile.Provider = "openaicompat"
	workflow, err := ProfileWorkflow(profile, prompt)
	if err != nil {
		t.Fatal(err)
	}
	return overload.ReviewSpec{Diff: diff, Workflow: workflow}
}

// focusedSpec builds a workflow whose agents share one model and differ only
// in a focus prompt, given as name and focus pairs.
func focusedSpec(t *testing.T, diff string, profile overload.ModelProfile, pairs ...string) overload.ReviewSpec {
	t.Helper()
	spec := profileSpec(t, diff, profile, "context")
	entry, model := spec.Workflow.Agents[0].EntryPrompt, spec.Workflow.Agents[0].Model
	spec.Workflow.Agents = nil
	for index := 0; index < len(pairs); index += 2 {
		body := entry.Body + "\n\nReview focus for " + pairs[index] + ":\n" + pairs[index+1]
		spec.Workflow.Agents = append(spec.Workflow.Agents, overload.ResolvedAgent{Name: pairs[index], Model: model, EntryPrompt: overload.PromptTemplate{Name: pairs[index], Kind: "entry", Body: body, SHA256: overload.PromptDigest(body)}})
	}
	return spec
}
