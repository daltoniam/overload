package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

type routingStore struct {
	automationStore
	resolved overload.ResolvedWorkflow
	routing  overload.Routing
	findings []overload.Finding
}

func (store *routingStore) ListFindings(context.Context, int64) ([]overload.Finding, error) {
	return store.findings, nil
}

func (store *routingStore) ResolveWorkflow(_ context.Context, name string) (overload.ResolvedWorkflow, error) {
	if name != store.resolved.Name {
		return overload.ResolvedWorkflow{}, http.ErrMissingFile
	}
	return store.resolved, nil
}

func (store *routingStore) ChangedFiles(_ context.Context, repository string, number int) ([]string, error) {
	if repository != "acme/api" || number != 7 {
		return nil, http.ErrMissingFile
	}
	return []string{"internal/auth/a.go", "main.go", "go.lock"}, nil
}

func (store *routingStore) ReadRunRouting(context.Context, int64) (overload.Routing, error) {
	return store.routing, nil
}

func TestWorkflowPreview(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	local := overload.ModelProfile{ConnectionKind: "local", BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen", Concurrency: 1}
	store := &routingStore{resolved: overload.ResolvedWorkflow{Version: overload.SnapshotVersion, Name: "review", Kind: "pr_review", SkipPaths: []string{"*.lock"}, MainReviews: overload.MainReviewsUnclaimed, Agents: []overload.ResolvedAgent{
		{Name: "lead", Model: local},
		{Name: "security", Model: local, Scope: overload.Scope{Paths: []string{"**/auth/**"}}},
	}}}
	handler := Handler(store)
	get := func(path string) (int, string) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response.Code, response.Body.String()
	}
	if code, body := get("/configure/workflows/review/preview"); code != http.StatusOK || !strings.Contains(body, "Changed files") || strings.Contains(body, "Model calls") {
		t.Fatalf("empty preview: %d %s", code, body)
	}
	code, body := get("/configure/workflows/review/preview?files=" + url.QueryEscape("internal/auth/a.go\nmain.go\ngo.lock\n"))
	for _, want := range []string{"Model calls", "about 16 min", "Skipped by skip paths: 1 file<", "security: 1 file<", "lead: 1 file<", "internal/auth/a.go"} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("preview missing %q: %d %s", want, code, body)
		}
	}
	if strings.Contains(body, "Tokens in / out") {
		t.Fatal("preview shows run results")
	}
	if code, body := get("/configure/workflows/review/preview?files=" + url.QueryEscape("../etc/passwd")); code != http.StatusOK || !strings.Contains(body, "invalid changed file path") {
		t.Fatalf("bad path: %d %s", code, body)
	}
	store.repos = []overload.Repository{{ID: 1, FullName: "acme/api"}}
	code, body = get("/configure/workflows/review/preview?repository=acme%2Fapi&pr=7")
	for _, want := range []string{"From a pull request", `<option value="acme/api" selected="">`, "security: 1 file<", "Skipped by skip paths: 1 file<", "main.go\ngo.lock"} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("PR preview missing %q: %d %s", want, code, body)
		}
	}
	if _, body := get("/configure/workflows/review/preview?repository=acme%2Fapi&pr=8"); !strings.Contains(body, "Could not load the pull request") {
		t.Fatalf("missing PR: %s", body)
	}
	if _, body := get("/configure/workflows/review/preview?repository=acme%2Fapi&pr=x"); !strings.Contains(body, "Choose a repository and a pull request number") {
		t.Fatalf("bad PR number: %s", body)
	}
	if code, _ := get("/configure/workflows/missing/preview"); code != http.StatusNotFound {
		t.Fatalf("missing workflow: %d", code)
	}
}

func TestRunDetailShowsRouting(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &routingStore{routing: overload.Routing{Skipped: []string{"go.lock"}, Planner: "planner added 1 file reviews", Degraded: []string{"sub-agent tests failed and did not review 1 of its 1 files"}, Agents: []overload.AgentFiles{{Agent: "lead", Files: []string{"main.go"}, Reviewed: 1}, {Agent: "security", Files: []string{"auth/a.go", "util/x.go"}, Planned: []string{"util/x.go"}, Reviewed: 2, Findings: 2, Capped: 1, Verified: 3, Dropped: 1, InputTokens: 900, OutputTokens: 40}, {Agent: "tests", Files: []string{"a_test.go"}, Failed: "bad request", Unreviewed: []string{"a_test.go"}}}}}
	store.findings = []overload.Finding{{Title: "Kept", Agents: []string{"security"}}, {Title: "Gone", DropReason: "verifier: not real"}}
	store.run = overload.Run{ID: 7, Kind: "pr_review", Status: overload.RunCompleted}
	response := httptest.NewRecorder()
	Handler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/runs/7", nil))
	body := response.Body.String()
	for _, want := range []string{"Agents and files", "Main agent", "Sub-agent", "900 / 40", "Skipped by skip paths: 1 file<", "Planner: planner added 1", "Partial review:", "3 / 1", "Added by the planner:", "Failed: bad request", "Reported by security", "Not posted. verifier: not real"} {
		if response.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("run detail missing %q: %d %s", want, response.Code, body)
		}
	}
}

func TestWorkflowTreeEditor(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &routingStore{}
	store.agents = []overload.AgentDefinition{{Name: "lead", Kind: "pr_review", Model: "qwen", Enabled: true}, {Name: "security", Kind: "pr_review", Model: "sol", Enabled: true}, {Name: "tests", Kind: "pr_review", Model: "qwen", Enabled: true}, {Name: "summary", Kind: "scheduled_prompt", Model: "qwen", Enabled: true}}
	store.prompts = []overload.PromptTemplate{{Name: "route", Kind: overload.PromptPlan, Body: "Plan.", Revision: 1}, {Name: "check", Kind: overload.PromptVerify, Body: "Verify.", Revision: 1}}
	store.workflows = []overload.Workflow{{Name: "review", Kind: "pr_review", Agents: []string{"lead", "security"}, Enabled: true, SkipPaths: []string{"*.lock"}, MaxFileReviews: 30, PlannerPrompt: "route", Scopes: map[string]overload.Scope{"security": {Paths: []string{"auth/**"}, Mode: overload.ScopeAlways, MaxFindings: 3}}}}
	handler := Handler(store)
	get := func(path string) string {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, response.Code)
		}
		return response.Body.String()
	}
	page := get("/configure/workflows/review")
	for _, want := range []string{`name="main_agent"`, `<option value="lead" selected="">`, `name="sub_0_agent"`, `<option value="security" selected="">`, `name="sub_7_agent"`, `<span class="chip">auth/**</span>`, `<span class="chip">only matching paths</span>`, `<span class="chip">at most 3 findings</span>`, `<option value="route" selected="">`, `<option value="check">`, `name="skip_paths"`, "*.lock", `value="30"`, "/configure/workflows/review/preview"} {
		if !strings.Contains(page, want) {
			t.Fatalf("editor missing %q\n%s", want, page)
		}
	}
	if strings.Contains(page, `value="summary"`) {
		t.Fatal("offered a scheduled-prompt agent in a PR workflow")
	}
	if list := get("/configure/workflows"); !strings.Contains(list, "security (auth/**)") {
		t.Fatal("workflow list does not show sub-agents and scopes")
	}
	switched := get("/configure/workflow-agents?kind=scheduled_prompt&main_agent=lead&sub_0_agent=summary")
	if strings.Contains(switched, "skip_paths") || strings.Contains(switched, "planner_prompt") || !strings.Contains(switched, `<option value="summary" selected>`) || strings.Contains(switched, `<option value="lead" selected>`) {
		t.Fatalf("kind switch: %s", switched)
	}

	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page)[1]
	post := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		form.Set("csrf", csrf)
		request := httptest.NewRequest(http.MethodPost, "/configure/workflows", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("HX-Request", "true")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	form := url.Values{"existing": {"review"}, "name": {"review"}, "kind": {"pr_review"}, "enabled": {"true"}, "main_agent": {"lead"}, "main_reviews": {"unclaimed"},
		"sub_0_agent": {"security"}, "sub_0_mode": {"globs"}, "sub_0_paths": {"**/auth/**\r\n\r\n**/*.sql\n"}, "sub_0_description": {"Auth and SQL"}, "sub_0_max_findings": {"5"},
		"sub_2_agent": {"tests"}, "sub_2_mode": {"planned"}, "sub_2_paths": {""}, "sub_2_max_findings": {""},
		"skip_paths": {"*.lock\nvendor/**"}, "max_file_reviews": {"60"}, "planner_prompt": {"route"}, "verifier_prompt": {"check"}}
	if response := post(form); response.Code != http.StatusOK || response.Header().Get("HX-Redirect") == "" {
		t.Fatalf("save: %d %s", response.Code, response.Body.String())
	}
	saved := store.workflows[0]
	want := overload.Workflow{Name: "review", Kind: "pr_review", Enabled: true, Agents: []string{"lead", "security", "tests"}, MainReviews: overload.MainReviewsUnclaimed, SkipPaths: []string{"*.lock", "vendor/**"}, MaxFileReviews: 60, PlannerPrompt: "route", VerifierPrompt: "check",
		Scopes: map[string]overload.Scope{"security": {Paths: []string{"**/auth/**", "**/*.sql"}, Description: "Auth and SQL", MaxFindings: 5}, "tests": {Mode: overload.ScopePlanned}}}
	if !reflect.DeepEqual(saved, want) {
		t.Fatalf("saved %+v\nwant  %+v", saved, want)
	}

	form.Set("planner_prompt", "")
	response := post(form)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "planned scopes need a planner prompt") {
		t.Fatalf("validation message: %d %s", response.Code, response.Body.String())
	}
	form.Set("planner_prompt", "route")
	form.Set("sub_0_max_findings", "lots")
	if response := post(form); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "max findings for security must be a number") {
		t.Fatalf("number message: %d %s", response.Code, response.Body.String())
	}
}
