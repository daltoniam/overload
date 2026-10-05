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

type fakeModel struct {
	mu       sync.Mutex
	reviews  []string
	planIn   string
	verified []string
	plan     string
	verdict  func(finding string) string
	fail     map[string]bool
}

func (model *fakeModel) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		system, user := request.Messages[0].Content, request.Messages[len(request.Messages)-1].Content
		path := ""
		if start := strings.Index(user, "diff --git a/"); start >= 0 {
			path = strings.Fields(user[start+len("diff --git a/"):])[0]
		}
		model.mu.Lock()
		var content string
		switch {
		case strings.HasPrefix(system, plannerPreamble):
			model.planIn = user
			content = model.plan
		case strings.HasPrefix(system, verifierPreamble):
			finding := user[strings.Index(user, "Finding to check:"):]
			model.verified = append(model.verified, request.Model+":"+path)
			content = model.verdict(finding)
		default:
			model.reviews = append(model.reviews, request.Model+":"+path)
			if model.fail[request.Model] {
				model.mu.Unlock()
				http.Error(w, "boom", http.StatusBadRequest)
				return
			}
			evidence := map[string]string{"util/helpers.go": "db.Query", "store/query.sql": "SELECT 1;", "main.go": "run()"}[path]
			content = fmt.Sprintf(`{"summary":"Issue","findings":[{"path":%q,"line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bug from %s","body":"Fix it","confidence":0.9,"evidence":%q}]}`, path, request.Model, evidence)
		}
		model.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":%q,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`, request.Model, content)
	}))
}

func plannerWorkflow(url string) overload.ResolvedWorkflow {
	prompt := func(kind, body string) overload.PromptTemplate {
		return overload.PromptTemplate{Name: kind, Kind: kind, Body: body, Revision: 1, SHA256: overload.PromptDigest(body)}
	}
	model := func(name string) overload.ModelProfile {
		return overload.ModelProfile{Provider: "openaicompat", BaseURL: url, Model: name}
	}
	planner, verifier := prompt(overload.PromptPlan, "Route SQL work to sql."), prompt(overload.PromptVerify, "Drop style nits.")
	return overload.ResolvedWorkflow{Version: overload.SnapshotVersion, Name: "planned", Kind: "pr_review", Revision: 1, MainReviews: overload.MainReviewsUnclaimed, PlannerPrompt: &planner, VerifierPrompt: &verifier, Agents: []overload.ResolvedAgent{
		{Name: "lead", Model: model("lead-model"), EntryPrompt: prompt("entry", "Review.")},
		{Name: "sql", Model: model("sql-model"), EntryPrompt: prompt("entry", "Review SQL."), Scope: overload.Scope{Paths: []string{"**/*.sql"}, Description: "SQL queries and database access"}},
		{Name: "auth", Model: model("auth-model"), EntryPrompt: prompt("entry", "Review auth."), Scope: overload.Scope{Paths: []string{"**/auth/**"}, Mode: overload.ScopeAlways}},
	}}
}

const plannedDiff = "diff --git a/util/helpers.go b/util/helpers.go\n+++ b/util/helpers.go\n@@ -1 +1 @@ func Lookup()\n-old\n+db.Query(\"SELECT * FROM users WHERE id=\" + id)\n" +
	"diff --git a/store/query.sql b/store/query.sql\n+++ b/store/query.sql\n@@ -1 +1 @@\n-old\n+SELECT 1;\n" +
	"diff --git a/main.go b/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+run()\n"

func TestPlannerRoutesMisleadingFileName(t *testing.T) {
	model := &fakeModel{plan: `{"assign":{"util/helpers.go":["sql"]}}`, verdict: func(finding string) string {
		if strings.Contains(finding, "store/query.sql") {
			return `{"decision":"drop","reason":"Not a real problem."}`
		}
		return `{"decision":"keep","reason":"Real injection."}`
	}}
	server := model.serve(t)
	defer server.Close()
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: plannedDiff, Workflow: plannerWorkflow(server.URL)}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(model.reviews)
	if strings.Join(model.reviews, ",") != "lead-model:main.go,sql-model:store/query.sql,sql-model:util/helpers.go" {
		t.Fatalf("reviews %v", model.reviews)
	}
	for _, want := range []string{"- sql: SQL queries and database access (already gets files matching **/*.sql)", "### util/helpers.go (+1 -1)", "@@ -1 +1 @@ func Lookup()", "+db.Query"} {
		if !strings.Contains(model.planIn, want) {
			t.Fatalf("planner input missing %q:\n%s", want, model.planIn)
		}
	}
	if strings.Contains(model.planIn, "- auth") || strings.Contains(model.planIn, "- lead") {
		t.Fatalf("planner offered agents it may not assign:\n%s", model.planIn)
	}
	slices.Sort(model.verified)
	if strings.Join(model.verified, ",") != "lead-model:store/query.sql,lead-model:util/helpers.go" {
		t.Fatalf("verifier must check sub-agent findings on the main model only: %v", model.verified)
	}
	if len(result.Findings) != 2 || len(result.Dropped) != 1 || result.Dropped[0].Path != "store/query.sql" || result.Dropped[0].DropReason != "verifier: Not a real problem." {
		t.Fatalf("findings %+v dropped %+v", result.Findings, result.Dropped)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	if !strings.Contains(routing.Planner, "added 1") || !slices.Equal(routing.Agents[1].Planned, []string{"util/helpers.go"}) || routing.Agents[1].Verified != 2 || routing.Agents[1].Dropped != 1 || routing.Agents[1].Findings != 1 || len(routing.Degraded) != 0 {
		t.Fatalf("routing %+v", routing)
	}
	if result.Metrics["planner_input_tokens"] != int64(10) || result.Metrics["degraded"] != nil {
		t.Fatalf("metrics %+v", result.Metrics)
	}
}

func TestPlannerFallsBackToGlobs(t *testing.T) {
	for name, reply := range map[string]string{
		"invalid JSON":     `not json`,
		"empty reply":      ``,
		"no assign":        `{"files":{}}`,
		"unknown file":     `{"assign":{"other.go":["sql"]}}`,
		"always sub-agent": `{"assign":{"main.go":["auth"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{plan: reply, verdict: func(string) string { return `{"decision":"keep","reason":"ok"}` }}
			server := model.serve(t)
			defer server.Close()
			result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: plannedDiff, Workflow: plannerWorkflow(server.URL)}, fstest.MapFS{})
			if err != nil {
				t.Fatal(err)
			}
			routing := result.Metrics["routing"].(overload.Routing)
			if !strings.Contains(routing.Planner, "routed by globs only") || len(routing.Agents[1].Planned) != 0 || len(routing.Agents[0].Files) != 2 {
				t.Fatalf("routing %+v", routing)
			}
		})
	}
}

func TestFailedSubAgentDegradesReview(t *testing.T) {
	model := &fakeModel{plan: `{"assign":{}}`, fail: map[string]bool{"sql-model": true}, verdict: func(string) string { return `{"decision":"maybe"}` }}
	server := model.serve(t)
	defer server.Close()
	workflow := plannerWorkflow(server.URL)
	workflow.Agents[2].Scope = overload.Scope{Paths: []string{"main.go"}, Mode: overload.ScopeAlways}
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: plannedDiff, Workflow: workflow}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	if routing.Agents[1].Failed == "" || len(routing.Agents[1].Unreviewed) != 1 || len(routing.Degraded) != 2 || result.Metrics["degraded"] != true {
		t.Fatalf("routing %+v", routing)
	}
	if !strings.Contains(routing.Degraded[0], "sub-agent sql failed") || !strings.Contains(routing.Degraded[1], "verifier could not check 1 findings") {
		t.Fatalf("degraded %q", routing.Degraded)
	}
	if len(result.Findings) != 2 || len(result.Dropped) != 0 {
		t.Fatalf("findings %+v", result.Findings)
	}

	model.fail = map[string]bool{"lead-model": true}
	if _, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: plannedDiff, Workflow: workflow}, fstest.MapFS{}); err == nil {
		t.Fatal("a failed main agent must fail the review")
	}
}
