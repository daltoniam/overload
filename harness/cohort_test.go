package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/daltoniam/overload"
)

// cohortServer answers every review with no findings and reports usage,
// recording which models were called.
func cohortServer(t *testing.T, tokens int) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		mu.Lock()
		calls = append(calls, request.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"summary\":\"ok\",\"findings\":[]}"}}],"usage":{"prompt_tokens":%d,"completion_tokens":0,"total_tokens":%d}}`, request.Model, tokens, tokens)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func cohortWorkflow(url string) overload.ResolvedWorkflow {
	prompt := func(body string) overload.PromptTemplate {
		return overload.PromptTemplate{Name: body, Kind: "entry", Body: body, Revision: 1, SHA256: overload.PromptDigest(body)}
	}
	model := func(name string) overload.ModelProfile {
		return overload.ModelProfile{Provider: "openaicompat", BaseURL: url, Model: name, Concurrency: 1}
	}
	return overload.ResolvedWorkflow{Version: overload.SnapshotVersion, Name: "cohort", Kind: "pr_review", Revision: 1, SensitivePaths: []string{"**/migrations/**"}, Agents: []overload.ResolvedAgent{
		{Name: "main", Model: model("main"), EntryPrompt: prompt("Review.")},
		{Name: "security", Model: model("security"), EntryPrompt: prompt("Security."), Scope: overload.Scope{Tiers: []string{overload.RiskFull}}},
		{Name: "tests", Model: model("tests"), EntryPrompt: prompt("Tests."), Scope: overload.Scope{Tiers: []string{overload.RiskLite, overload.RiskFull}}},
	}}
}

func lineDiff(path string, lines int) string {
	var diff strings.Builder
	fmt.Fprintf(&diff, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, path, lines)
	for index := range lines {
		fmt.Fprintf(&diff, "+line%d()\n", index)
	}
	return diff.String()
}

func TestRiskTiersGateSubAgents(t *testing.T) {
	server, calls := cohortServer(t, 10)
	for _, test := range []struct {
		name, diff, tier string
		want             string
	}{
		{"trivial", lineDiff("a.go", 5), overload.RiskTrivial, "main"},
		{"lite", lineDiff("a.go", 50), overload.RiskLite, "main,tests"},
		{"sensitive", lineDiff("db/migrations/001.sql", 3), overload.RiskFull, "main,security,tests"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(calls())
			result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: test.diff, Workflow: cohortWorkflow(server.URL)}, fstest.MapFS{})
			if err != nil {
				t.Fatal(err)
			}
			got := calls()[before:]
			seen := map[string]bool{}
			var models []string
			for _, model := range []string{"main", "security", "tests"} {
				for _, call := range got {
					if call == model && !seen[model] {
						seen[model] = true
						models = append(models, model)
					}
				}
			}
			routing := result.Metrics["routing"].(overload.Routing)
			if strings.Join(models, ",") != test.want || routing.Tier != test.tier {
				t.Fatalf("models %v tier %s", models, routing.Tier)
			}
			if test.tier == overload.RiskTrivial && !routing.Agents[1].OutOfTier {
				t.Fatal("security not marked out of tier")
			}
		})
	}
}

func TestReviewTokenBudget(t *testing.T) {
	server, calls := cohortServer(t, 1000)
	workflow := cohortWorkflow(server.URL)
	workflow.Agents, workflow.SensitivePaths, workflow.MaxReviewTokens = workflow.Agents[:1], nil, 1500
	diff := lineDiff("a.go", 3) + lineDiff("b.go", 3) + lineDiff("c.go", 3)
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: workflow}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	if len(calls()) != 2 || len(routing.Agents[0].Unreviewed) != 1 || !strings.Contains(routing.Agents[0].Failed, "budget") || len(routing.Degraded) != 1 {
		t.Fatalf("calls %v routing %+v", calls(), routing)
	}
	if gate := overload.GateOutcome(overload.GateInput{Routing: routing, Degraded: true}); gate.State != overload.GateFailure {
		t.Fatalf("a review cut short by its budget passed: %+v", gate)
	}
}

func TestChangedLinesCountsOnlyHunkContent(t *testing.T) {
	for _, test := range []struct {
		name  string
		patch string
		want  int
	}{
		{"headers only", "diff --git a/a.sql b/a.sql\n--- a/a.sql\n+++ b/a.sql\n", 0},
		{"plain", "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n context\n", 2},
		{"sql comment removed", "--- a/a.sql\n+++ b/a.sql\n@@ -1,2 +1 @@\n--- old sql comment\n-- kept\n", 2},
		{"content starting with ++", "--- a/a.txt\n+++ b/a.txt\n@@ -0,0 +1,2 @@\n+++x\n++y\n", 2},
		{"two hunks", "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-a\n+b\n@@ -9 +9 @@\n-c\n+d\n\\ No newline at end of file\n", 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := changedLines(test.patch); got != test.want {
				t.Fatalf("changedLines = %d, want %d", got, test.want)
			}
		})
	}
}

func TestOutOfTierAgentsNeitherClaimNorGetPlannedFiles(t *testing.T) {
	server, calls := cohortServer(t, 10)
	workflow := cohortWorkflow(server.URL)
	planner := overload.PromptTemplate{Name: "plan", Kind: overload.PromptPlan, Body: "Plan.", Revision: 1, SHA256: overload.PromptDigest("Plan.")}
	workflow.PlannerPrompt, workflow.MainReviews = &planner, overload.MainReviewsUnclaimed
	workflow.Agents = workflow.Agents[:2]
	workflow.Agents[1].Scope.Paths = []string{"**"}
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: lineDiff("a.go", 3), Workflow: workflow}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	// One main call: the file review. A planner call would be a second.
	if got := strings.Join(calls(), ","); got != "main" || routing.Tier != overload.RiskTrivial || !routing.Agents[1].OutOfTier || len(routing.Agents[0].Files) != 1 {
		t.Fatalf("calls %s routing %+v", got, routing)
	}
}

func TestReviewTokenBudgetCountsThePlanner(t *testing.T) {
	server, calls := cohortServer(t, 1000)
	workflow := cohortWorkflow(server.URL)
	planner := overload.PromptTemplate{Name: "plan", Kind: overload.PromptPlan, Body: "Plan.", Revision: 1, SHA256: overload.PromptDigest("Plan.")}
	workflow.PlannerPrompt, workflow.SensitivePaths, workflow.MaxReviewTokens = &planner, nil, 1500
	workflow.Agents = []overload.ResolvedAgent{workflow.Agents[0], {Name: "tests", Model: workflow.Agents[2].Model, EntryPrompt: workflow.Agents[2].EntryPrompt, Scope: overload.Scope{Paths: []string{"**/*_test.go"}}}}
	diff := lineDiff("a.go", 3) + lineDiff("b.go", 3) + lineDiff("c.go", 3)
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: workflow}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	if len(calls()) != 2 || len(routing.Agents[0].Unreviewed) != 2 || !strings.Contains(routing.Agents[0].Failed, "budget") {
		t.Fatalf("calls %v routing %+v", calls(), routing)
	}
}

func TestReviewTokenBudgetStopsTheVerifier(t *testing.T) {
	var mu sync.Mutex
	verifierCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		content := `{"summary":"ok","findings":[]}`
		switch {
		case strings.Contains(string(body), "You check one finding"):
			mu.Lock()
			verifierCalls++
			mu.Unlock()
			content = `{"decision":"drop","reason":"style"}`
		case request.Model == "security":
			content = `{"summary":"bug","findings":[{"path":"a.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bad call","body":"line0 is wrong","confidence":0.9,"evidence":"line0()"}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":1000,"completion_tokens":0,"total_tokens":1000}}`, request.Model, content)
	}))
	defer server.Close()
	workflow := cohortWorkflow(server.URL)
	verifier := overload.PromptTemplate{Name: "verify", Kind: overload.PromptVerify, Body: "Verify.", Revision: 1, SHA256: overload.PromptDigest("Verify.")}
	workflow.VerifierPrompt, workflow.SensitivePaths, workflow.MaxReviewTokens = &verifier, nil, 2000
	workflow.Agents = workflow.Agents[:2]
	workflow.Agents[1].Scope.Tiers = nil
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: lineDiff("a.go", 3), Workflow: workflow}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	if verifierCalls != 0 || len(result.Findings) != 1 || len(routing.Degraded) != 1 || !strings.Contains(routing.Degraded[0], "before the verifier checked 1") {
		t.Fatalf("verifier calls %d findings %+v degraded %v", verifierCalls, result.Findings, routing.Degraded)
	}
}
