package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
)

type fakeStore struct {
	delivered int
	settings  []overload.ReviewSettings
	run       overload.Run
	output    postgres.JobOutput
}

func (store *fakeStore) ListReviewSettings(context.Context) ([]overload.ReviewSettings, error) {
	return store.settings, nil
}
func (store *fakeStore) SaveReviewSettings(_ context.Context, setting overload.ReviewSettings) error {
	for index := range store.settings {
		if store.settings[index].Name == setting.Name {
			store.settings[index] = setting
			return nil
		}
	}
	store.settings = append(store.settings, setting)
	return nil
}
func (store *fakeStore) DeleteReviewSettings(_ context.Context, name string) error {
	for index, setting := range store.settings {
		if setting.Name == name {
			store.settings = append(store.settings[:index], store.settings[index+1:]...)
			return nil
		}
	}
	return nil
}

func (store *fakeStore) ListRuns(context.Context) ([]overload.Run, error) { return nil, nil }
func (store *fakeStore) GetRun(context.Context, int64) (overload.Run, error) {
	return store.run, nil
}
func (store *fakeStore) ReadJobOutput(context.Context, int64) (postgres.JobOutput, error) {
	return store.output, nil
}
func (store *fakeStore) ListRunEvents(context.Context, int64) ([]postgres.RunEvent, error) {
	return nil, nil
}
func (store *fakeStore) ListDeliveries(context.Context) ([]postgres.Delivery, error) { return nil, nil }
func (store *fakeStore) ListFindings(context.Context, int64) ([]overload.Finding, error) {
	return nil, nil
}
func (store *fakeStore) ReadArtifact(context.Context, int64, string) ([]byte, error) {
	return []byte(`{}`), nil
}
func (store *fakeStore) RecordDelivery(context.Context, string, string, string, string, []byte) (bool, error) {
	store.delivered++
	return true, nil
}

type fakeConfigStore struct{ fakeStore }

func (store *fakeConfigStore) ListAgents(context.Context) ([]overload.AgentDefinition, error) {
	return nil, nil
}
func (store *fakeConfigStore) SaveAgent(context.Context, overload.AgentDefinition) error { return nil }
func (store *fakeConfigStore) DeleteAgent(context.Context, string) error                 { return nil }
func (store *fakeConfigStore) ListWorkflows(context.Context) ([]overload.Workflow, error) {
	return nil, nil
}
func (store *fakeConfigStore) ResolveWorkflow(context.Context, string) (overload.ResolvedWorkflow, error) {
	return overload.ResolvedWorkflow{}, nil
}
func (store *fakeConfigStore) SaveWorkflow(context.Context, overload.Workflow) error { return nil }
func (store *fakeConfigStore) DeleteWorkflow(context.Context, string) error          { return nil }
func (store *fakeConfigStore) ListBindings(context.Context) ([]overload.TriggerBinding, error) {
	return nil, nil
}
func (store *fakeConfigStore) SaveBinding(context.Context, overload.TriggerBinding) error { return nil }
func (store *fakeConfigStore) DeleteBinding(context.Context, int64, string) error         { return nil }
func (store *fakeConfigStore) ListRepositories(context.Context) ([]overload.Repository, error) {
	return nil, nil
}
func (store *fakeConfigStore) SaveRepository(context.Context, string, bool, bool) error { return nil }
func (store *fakeConfigStore) DeleteRepository(context.Context, int64) error            { return nil }
func (store *fakeConfigStore) ListSchedules(context.Context) ([]overload.Schedule, error) {
	return nil, nil
}
func (store *fakeConfigStore) SaveSchedule(context.Context, overload.Schedule) error { return nil }
func (store *fakeConfigStore) DeleteSchedule(context.Context, string) error          { return nil }

func TestConfigureAuthAndCSRF(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "")
	t.Setenv("OVERLOAD_UI_USER", "admin")
	t.Setenv("OVERLOAD_UI_PASSWORD", "password")
	handler := Handler(&fakeConfigStore{})
	request := httptest.NewRequest(http.MethodGet, "/configure", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized: %d", response.Code)
	}
	request.SetBasicAuth("admin", "password")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("configuration redirect: %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("admin", "password")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Run statistics") || strings.Contains(response.Body.String(), `href="/configure"`) || !strings.Contains(response.Body.String(), `class="sidebar"`) {
		t.Fatalf("overview navigation: %d", response.Code)
	}
	form := url.Values{"name": {"security"}, "model": {"local"}, "prompt": {"Check authorization."}, "enabled": {"true"}}
	post := httptest.NewRequest(http.MethodPost, "/configure/agents", strings.NewReader(form.Encode()))
	post.SetBasicAuth("admin", "password")
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, post)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF accepted: %d", response.Code)
	}
	match := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(response.Body.String())
	if len(match) != 2 {
		page := httptest.NewRecorder()
		promptRequest := httptest.NewRequest(http.MethodGet, "/configure/agents/new", nil)
		promptRequest.SetBasicAuth("admin", "password")
		handler.ServeHTTP(page, promptRequest)
		match = regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())
	}
	if len(match) != 2 {
		t.Fatal("configuration form token missing")
	}
	form.Set("csrf", match[1])
	post = httptest.NewRequest(http.MethodPost, "/configure/agents", strings.NewReader(form.Encode()))
	post.SetBasicAuth("admin", "password")
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, post)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("valid agent form: %d %s", response.Code, response.Body.String())
	}
}

func TestRunDetailAndArtifact(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	handler := Handler(&fakeStore{})
	for _, test := range []struct {
		path string
		want int
	}{
		{"/runs/1", http.StatusOK},
		{"/runs/1/artifacts/result.json", http.StatusOK},
		{"/runs/1/artifacts/other", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.want {
			t.Errorf("%s: got %d want %d", test.path, response.Code, test.want)
		}
	}
}

func TestScheduledRunDetail(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &fakeStore{run: overload.Run{ID: 42, Kind: "scheduled_prompt", Trigger: "cron", Status: "completed"}, output: postgres.JobOutput{Kind: "scheduled_prompt", Agents: []postgres.AgentOutput{{Name: "summarizer", Text: "<script>unsafe</script> summary"}}}}
	response := httptest.NewRecorder()
	Handler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/runs/42", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "Scheduled output") || !strings.Contains(body, "summarizer") || !strings.Contains(body, "&lt;script&gt;unsafe") || strings.Contains(body, "Pull request #") || strings.Contains(body, "Download input") {
		t.Fatalf("scheduled detail: %d %s", response.Code, body)
	}
}

func TestSettingsFormAuthorizationAndCSRF(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "")
	t.Setenv("OVERLOAD_UI_USER", "admin")
	t.Setenv("OVERLOAD_UI_PASSWORD", "password")
	store := &fakeStore{}
	handler := Handler(store)
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status: %d", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/settings/new", nil)
	request.SetBasicAuth("admin", "password")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("settings status: %d", response.Code)
	}
	token := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(response.Body.String())
	if len(token) != 2 {
		t.Fatal("settings form token missing")
	}
	form := url.Values{"name": {"local"}, "base_url": {"http://127.0.0.1:8080/v1"}, "model": {"bonsai-2-27b"}, "connection_kind": {"local"}, "csrf": {token[1]}}
	for _, test := range []struct {
		name   string
		origin string
		token  string
		want   int
	}{
		{"cross origin", "https://other.example", token[1], http.StatusForbidden},
		{"missing token", "", "", http.StatusForbidden},
		{"valid form", "http://example.com", token[1], http.StatusSeeOther},
	} {
		t.Run(test.name, func(t *testing.T) {
			form.Set("csrf", test.token)
			request := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			request.SetBasicAuth("admin", "password")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d", response.Code, test.want)
			}
		})
	}
	if len(store.settings) != 1 || store.settings[0].Model != "bonsai-2-27b" || store.settings[0].ConnectionKind != "local" {
		t.Fatalf("saved settings: %+v", store.settings)
	}
}

func TestWebhookSignatureRequired(t *testing.T) {
	t.Setenv("GITHUB_WEBHOOK_SECRET", "test-secret")
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &fakeStore{}
	handler := Handler(store)
	body := `{"action":"opened","number":1,"repository":{"full_name":"x/y"}}`
	for _, tc := range []struct {
		valid bool
		want  int
	}{{false, http.StatusUnauthorized}, {true, http.StatusAccepted}} {
		request := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
		request.Header.Set("X-GitHub-Delivery", "delivery-1")
		request.Header.Set("X-GitHub-Event", "pull_request")
		if tc.valid {
			mac := hmac.New(sha256.New, []byte("test-secret"))
			_, _ = mac.Write([]byte(body))
			request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.want {
			t.Fatalf("status %d, want %d", response.Code, tc.want)
		}
	}
	if store.delivered != 1 {
		t.Fatalf("recorded %d deliveries, want one", store.delivered)
	}
}
