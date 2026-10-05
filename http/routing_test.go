package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

type routingStore struct {
	automationStore
	resolved overload.ResolvedWorkflow
	routing  overload.Routing
}

func (store *routingStore) ResolveWorkflow(_ context.Context, name string) (overload.ResolvedWorkflow, error) {
	if name != store.resolved.Name {
		return overload.ResolvedWorkflow{}, http.ErrMissingFile
	}
	return store.resolved, nil
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
	for _, want := range []string{"Model calls", "about 16 min", "Skipped by skip paths: 1 files", "security: 1 files", "lead: 1 files", "internal/auth/a.go"} {
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
	if code, _ := get("/configure/workflows/missing/preview"); code != http.StatusNotFound {
		t.Fatalf("missing workflow: %d", code)
	}
}

func TestRunDetailShowsRouting(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &routingStore{routing: overload.Routing{Skipped: []string{"go.lock"}, Agents: []overload.AgentFiles{{Agent: "lead", Files: []string{"main.go"}, Reviewed: 1}, {Agent: "security", Files: []string{"auth/a.go"}, Reviewed: 1, Findings: 2, Capped: 1, InputTokens: 900, OutputTokens: 40}}}}
	store.run = overload.Run{ID: 7, Kind: "pr_review", Status: overload.RunCompleted}
	response := httptest.NewRecorder()
	Handler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/runs/7", nil))
	body := response.Body.String()
	for _, want := range []string{"Agents and files", "Main agent", "Sub-agent", "900 / 40", "Skipped by skip paths: 1 files"} {
		if response.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("run detail missing %q: %d %s", want, response.Code, body)
		}
	}
}

func TestWorkflowFormKeepsRouting(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &routingStore{}
	store.workflows = []overload.Workflow{{Name: "review", Kind: "pr_review", Agents: []string{"lead", "security"}, Enabled: true, SkipPaths: []string{"*.lock"}, MaxFileReviews: 30, Scopes: map[string]overload.Scope{"security": {Paths: []string{"auth/**"}}}}}
	handler := Handler(store)
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/configure/workflows/review", nil))
	match := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 || !strings.Contains(page.Body.String(), "/configure/workflows/review/preview") {
		t.Fatal("missing CSRF token or preview link")
	}
	form := url.Values{"csrf": {match[1]}, "existing": {"review"}, "name": {"review"}, "kind": {"pr_review"}, "agents": {"lead", "security"}, "enabled": {"true"}}
	request := httptest.NewRequest(http.MethodPost, "/configure/workflows", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	saved := store.workflows[0]
	if response.Code != http.StatusSeeOther || saved.MaxFileReviews != 30 || saved.SkipPaths[0] != "*.lock" || saved.Scopes["security"].Paths[0] != "auth/**" {
		t.Fatalf("routing lost on save: %d %+v", response.Code, saved)
	}
}
