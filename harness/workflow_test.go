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

func TestWorkflowRoutesFilesByScope(t *testing.T) {
	var mu sync.Mutex
	reviewed := []string{}
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
		path := "b.go"
		if strings.Contains(text, "diff --git a/auth/a.go") {
			path = "auth/a.go"
		}
		if strings.Contains(text, "go.lock") {
			t.Error("skipped file sent to a model")
		}
		mu.Lock()
		reviewed = append(reviewed, request.Model+":"+path)
		mu.Unlock()
		content := `{"summary":"None","findings":[]}`
		if path == "auth/a.go" {
			var findings []string
			for index, severity := range []string{"low", "critical", "medium"} {
				findings = append(findings, fmt.Sprintf(`{"path":"auth/a.go","line":%d,"side":"RIGHT","severity":%q,"category":"security","title":"Issue %d","body":"Fix it","confidence":0.9,"evidence":"bad%d()"}`, index+1, severity, index+1, index+1))
			}
			content = `{"summary":"Issues","findings":[` + strings.Join(findings, ",") + `]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":%q,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`, request.Model, content)
	}))
	defer server.Close()
	prompt := overload.PromptTemplate{Name: "entry", Kind: "entry", Body: "Review.", Revision: 1, SHA256: overload.PromptDigest("Review.")}
	model := func(name string) overload.ModelProfile {
		return overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: name}
	}
	workflow := overload.ResolvedWorkflow{Version: overload.SnapshotVersion, Name: "routed", Kind: "pr_review", Revision: 1, SkipPaths: []string{"*.lock"}, MainReviews: overload.MainReviewsUnclaimed, Agents: []overload.ResolvedAgent{
		{Name: "lead", Model: model("lead-model"), EntryPrompt: prompt},
		{Name: "security", Model: model("security-model"), EntryPrompt: prompt, Scope: overload.Scope{Paths: []string{"**/auth/**"}, MaxFindings: 1}},
	}}
	lock := "diff --git a/go.lock b/go.lock\n+++ b/go.lock\n@@ -1 +1 @@\n-old\n+" + strings.Repeat("x", 40000) + "\n"
	diff := "diff --git a/auth/a.go b/auth/a.go\n+++ b/auth/a.go\n@@ -1 +1,3 @@\n-old\n+bad1()\n+bad2()\n+bad3()\n" +
		"diff --git a/b.go b/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-old\n+ok()\n" + lock
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: workflow}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(reviewed)
	if strings.Join(reviewed, ",") != "lead-model:b.go,security-model:auth/a.go" {
		t.Fatalf("reviews %v", reviewed)
	}
	if len(result.Findings) != 1 || result.Findings[0].Severity != "critical" || result.Findings[0].Agents[0] != "security" {
		t.Fatalf("findings %+v", result.Findings)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	if len(routing.Skipped) != 1 || routing.Agents[1].Capped != 2 || routing.Agents[1].Findings != 1 || routing.Agents[0].Reviewed != 1 || routing.Agents[1].InputTokens == 0 || result.Metrics["skipped_files"] != 1 {
		t.Fatalf("routing %+v metrics %+v", routing, result.Metrics)
	}

	onlyLock := overload.ReviewSpec{Diff: lock, Workflow: workflow}
	result, err = (Reviewer{}).Review(context.Background(), onlyLock, fstest.MapFS{})
	if err != nil || len(result.Findings) != 0 || !strings.Contains(result.Summary, "skip paths") {
		t.Fatalf("all skipped: %+v %v", result, err)
	}

	workflow.MaxFileReviews = 1
	if _, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: workflow}, fstest.MapFS{}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("file review limit: %v", err)
	}
}
