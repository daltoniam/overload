package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/daltoniam/overload"
)

func TestResolvedWorkflowUsesDistinctModelsAndPrompts(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		text := fmt.Sprint(request.Messages)
		if (request.Model == "security-model" && (!strings.Contains(text, "Security entry") || !strings.Contains(text, "Security review"))) || (request.Model == "correctness-model" && (!strings.Contains(text, "Correctness entry") || !strings.Contains(text, "Correctness review"))) {
			t.Errorf("wrong prompt for model %s", request.Model)
		}
		mu.Lock()
		calls = append(calls, request.Model)
		mu.Unlock()
		content := `{"summary":"Issue","findings":[{"path":"a.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bug","body":"Fix it","confidence":0.9,"evidence":"bad()"}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":%q,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, request.Model, content)
	}))
	defer server.Close()
	makePrompt := func(name, kind, body string) overload.PromptTemplate {
		return overload.PromptTemplate{Name: name, Kind: kind, Body: body, Revision: 1, SHA256: overload.PromptDigest(body)}
	}
	workflow := overload.ResolvedWorkflow{Name: "test-workflow", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{
		{Name: "security", Model: overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "security-model"}, EntryPrompt: makePrompt("security", "entry", "Security entry"), ReviewPrompt: makePrompt("security", "review", "Security review")},
		{Name: "correctness", Model: overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "correctness-model"}, EntryPrompt: makePrompt("correctness", "entry", "Correctness entry"), ReviewPrompt: makePrompt("correctness", "review", "Correctness review")},
	}}
	spec := overload.ReviewSpec{Diff: "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+bad()\n", Workflow: workflow}
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
	slices.Sort(calls)
	if err != nil || strings.Join(calls, ",") != "correctness-model,security-model" || len(result.Findings) != 1 {
		t.Fatalf("calls=%v result=%+v error=%v", calls, result, err)
	}
	if got := strings.Join(result.Findings[0].Agents, ","); got != "security,correctness" {
		t.Fatalf("finding reported by both agents should list both in workflow order, got %q", got)
	}
}
