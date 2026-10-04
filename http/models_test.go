package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

func TestModelCRUD(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &fakeStore{settings: []overload.ReviewSettings{{Name: "legacy", Provider: "openaicompat", ConnectionKind: "local", BaseURL: "http://127.0.0.1:8080/v1", Model: "old", PromptProfile: "switchboard-go"}}}
	handler := Handler(store)
	page := func(path string, status int) string {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != status {
			t.Fatalf("%s: status %d, want %d", path, response.Code, status)
		}
		return response.Body.String()
	}
	listing := page("/settings", http.StatusOK)
	if !strings.Contains(listing, `href="/settings/legacy"`) || strings.Contains(listing, "Focused review passes") {
		t.Fatal("models list is not separate from legacy passes")
	}
	body := page("/settings/legacy", http.StatusOK)
	if strings.Contains(body, "Focused review passes") || !strings.Contains(body, `value="legacy"`) || !strings.Contains(body, `class="danger"`) {
		t.Fatal("model edit form is not connection-only")
	}
	if !strings.Contains(page("/settings/new", http.StatusOK), "Hosted API") {
		t.Fatal("hosted API option missing")
	}
	page("/settings/missing", http.StatusNotFound)
	match := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("missing model form token")
	}
	post := func(path string, form url.Values, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s: status %d, want %d: %s", path, response.Code, want, response.Body.String())
		}
	}
	csrf := match[1]
	post("/settings", url.Values{"csrf": {csrf}, "existing": {"legacy"}, "name": {"other"}, "connection_kind": {"local"}, "model": {"new"}, "base_url": {"http://127.0.0.1:8080/v1"}}, http.StatusBadRequest)
	post("/settings", url.Values{"csrf": {csrf}, "existing": {"legacy"}, "name": {"legacy"}, "connection_kind": {"hosted"}, "model": {"new"}, "base_url": {"https://example.com/v1"}}, http.StatusBadRequest)
	post("/settings", url.Values{"csrf": {csrf}, "existing": {"legacy"}, "name": {"legacy"}, "connection_kind": {"hosted"}, "model": {"new"}, "base_url": {"https://example.com/v1"}, "api_key_env": {"MODEL_KEY"}, "concurrency": {"40"}}, http.StatusBadRequest)
	post("/settings", url.Values{"csrf": {csrf}, "existing": {"legacy"}, "name": {"legacy"}, "connection_kind": {"hosted"}, "model": {"new"}, "base_url": {"https://example.com/v1"}, "api_key_env": {"MODEL_KEY"}, "concurrency": {"6"}, "reasoning_param": {"reasoning_effort"}, "reasoning_effort": {"turbo"}}, http.StatusBadRequest)
	post("/settings", url.Values{"csrf": {csrf}, "existing": {"legacy"}, "name": {"legacy"}, "connection_kind": {"hosted"}, "model": {"new"}, "base_url": {"https://example.com/v1"}, "api_key_env": {"MODEL_KEY"}, "concurrency": {"6"}, "reasoning_param": {"reasoning_effort"}, "reasoning_effort": {"high"}, "max_output_tokens": {"8192"}}, http.StatusSeeOther)
	if store.settings[0].Concurrency != 6 || store.settings[0].ReasoningEffort != "high" || store.settings[0].MaxOutputTokens != 8192 {
		t.Fatalf("concurrency not saved: %+v", store.settings[0])
	}
	if store.settings[0].Model != "new" || store.settings[0].ConnectionKind != "hosted" || store.settings[0].PromptProfile != "switchboard-go" {
		t.Fatalf("model update lost legacy configuration: %+v", store.settings[0])
	}
	post("/settings/legacy/delete", url.Values{}, http.StatusForbidden)
	post("/settings/legacy/delete", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusSeeOther)
	if len(store.settings) != 0 {
		t.Fatal("model deletion failed")
	}
}
