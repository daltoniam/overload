package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
)

type uiSearchStore struct {
	fakeStore
	filter postgres.ListFilter
}

func (store *uiSearchStore) SearchRuns(_ context.Context, filter postgres.ListFilter) ([]overload.Run, int, error) {
	store.filter = filter
	return []overload.Run{{ID: 142, Kind: "pr_review", Status: overload.RunFailed}}, 125, nil
}

func (store *uiSearchStore) SearchDeliveries(_ context.Context, filter postgres.ListFilter) ([]postgres.Delivery, int, error) {
	store.filter = filter
	return []postgres.Delivery{{Repository: "example/repo", Event: "pull_request", Outcome: "skipped"}}, 50, nil
}

func TestUISearchFragments(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &uiSearchStore{}
	handler := Handler(store)
	for _, path := range []string{"/runs?q=old&status=failed&page=2", "/webhooks?q=example&status=skipped&page=2"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("HX-Request", "true")
		request.Header.Set("HX-Target", "list-results")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 || strings.Contains(response.Body.String(), "<html") || !strings.Contains(response.Body.String(), "matching records") {
			t.Fatalf("unexpected fragment: %d %s", response.Code, response.Body.String())
		}
		if store.filter.Page != 2 || store.filter.Query == "" {
			t.Fatalf("missing database search: %+v", store.filter)
		}
		request.Header.Set("HX-History-Restore-Request", "true")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if !strings.Contains(response.Body.String(), "<html") {
			t.Fatal("history restoration must receive a full page")
		}
	}
}

func TestUIModelSearchModalAndErrors(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &fakeStore{settings: []overload.ReviewSettings{{Name: "local-bonsai", ConnectionKind: "local", Model: "bonsai", BaseURL: "http://127.0.0.1:8080/v1"}, {Name: "hosted-review", ConnectionKind: "hosted", Model: "gpt", BaseURL: "https://example.com/v1"}}}
	handler := Handler(store)
	request := httptest.NewRequest(http.MethodGet, "/settings?q=bonsai&kind=local", nil)
	request.Header.Set("HX-Request", "true")
	request.Header.Set("HX-Target", "list-results")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), "local-bonsai") || strings.Contains(response.Body.String(), "hosted-review") || strings.Contains(response.Body.String(), "<html") {
		t.Fatal("model search is not a filtered fragment")
	}
	request = httptest.NewRequest(http.MethodGet, "/settings/new", nil)
	request.Header.Set("HX-Request", "true")
	request.Header.Set("HX-Target", "modal-root")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), "<dialog") || strings.Contains(response.Body.String(), "<html") {
		t.Fatal("missing model dialog")
	}
	match := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(response.Body.String())
	if len(match) != 2 {
		t.Fatal("missing token")
	}
	form := url.Values{"csrf": {match[1]}, "name": {"bad-model"}, "connection_kind": {"hosted"}, "base_url": {"https://example.com/v1"}, "model": {"keep-my-model"}}
	request = httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 || !strings.Contains(response.Body.String(), `value="keep-my-model"`) || !strings.Contains(response.Body.String(), "Invalid model settings") {
		t.Fatal("native validation lost form input")
	}
	request = httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("HX-Request", "true")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Header().Get("X-UI-Validation") != "true" || strings.Contains(response.Body.String(), "<html") {
		t.Fatal("enhanced validation must retain existing form")
	}
	request = httptest.NewRequest(http.MethodPost, "/settings/local-bonsai/delete", strings.NewReader(url.Values{"csrf": {match[1]}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || len(store.settings) != 2 || !strings.Contains(response.Body.String(), "Confirm deletion") {
		t.Fatal("unconfirmed delete mutated store")
	}
}

func TestUIAssetPolicy(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	handler := Handler(&fakeStore{})
	for _, path := range []string{"/assets/ui.js", "/assets/ui.css", "/assets/htmx.min.js"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 200 || response.Body.Len() == 0 {
			t.Fatalf("asset missing: %s", path)
		}
		policy := response.Header().Get("Content-Security-Policy")
		if !strings.Contains(policy, "connect-src 'self'") || strings.Contains(policy, "unsafe-eval") {
			t.Fatal("unsafe or unusable CSP")
		}
	}
}

func TestUIPreview(t *testing.T) {
	if os.Getenv("OVERLOAD_UI_PREVIEW") != "1" {
		t.Skip("local screenshot preview only")
	}
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	if os.Getenv("OVERLOAD_UI_DATABASE") != "" {
		store, err := postgres.Open(context.Background(), os.Getenv("OVERLOAD_UI_DATABASE"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Pool.Close()
		server := &http.Server{Addr: "127.0.0.1:8082", Handler: Handler(store)}
		if err := server.ListenAndServe(); err != nil {
			t.Fatal(err)
		}
		return
	}
	store := &automationStore{}
	store.settings = []overload.ReviewSettings{{Name: "local-bonsai", ConnectionKind: "local", Model: "bonsai-2-27b", BaseURL: "http://127.0.0.1:8080/v1", Concurrency: 1, IsDefault: true}, {Name: "hosted-review", ConnectionKind: "hosted", Model: "gpt-4o", BaseURL: "https://example.com/v1", Concurrency: 1}}
	store.agents = []overload.AgentDefinition{{Name: "review-agent", Kind: "pr_review", Model: "local-bonsai", Prompt: "Review the pull request.\nReport actionable findings.", PromptRevision: 3, Enabled: true}, {Name: "summary-agent", Kind: "scheduled_prompt", Model: "hosted-review", Prompt: "Summarize the status.", PromptRevision: 1, Enabled: true}}
	store.workflows = []overload.Workflow{{Name: "pr-review", Kind: "pr_review", Agents: []string{"review-agent"}, Enabled: true}, {Name: "daily-summary", Kind: "scheduled_prompt", Agents: []string{"summary-agent"}, Enabled: true}}
	store.repos = []overload.Repository{{ID: 1, FullName: "example/repository", Enabled: true, DryRun: true}}
	store.schedules = []overload.Schedule{{Name: "demo-summary", Workflow: "daily-summary", Cron: "0 9 * * *", Timezone: "UTC"}}
	store.run = overload.Run{ID: 599, Kind: "pr_review", Status: overload.RunFailed, PRNumber: 42, Trigger: "webhook", HeadSHA: "demo-head", ErrorMessage: "GitHub credentials unavailable to worker"}
	server := &http.Server{Addr: "127.0.0.1:8082", Handler: Handler(store)}
	if err := server.ListenAndServe(); err != nil {
		t.Fatal(err)
	}
}
