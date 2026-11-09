package harness

import (
	"context"
	"encoding/json"
	"fmt"
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
