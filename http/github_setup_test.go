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

	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
)

type fakeAppStore struct {
	fakeStore
	app           *github.AppCredentials
	installations []github.InstallationEvent
}

func (store *fakeAppStore) LoadGitHubApp(context.Context) (github.AppCredentials, error) {
	if store.app == nil {
		return github.AppCredentials{}, postgres.ErrNoGitHubApp
	}
	return *store.app, nil
}
func (store *fakeAppStore) SaveGitHubApp(_ context.Context, app github.AppCredentials) error {
	store.app = &app
	return nil
}
func (store *fakeAppStore) GitHubWebhookSecret(context.Context) (string, error) {
	if store.app == nil {
		return "", nil
	}
	return store.app.WebhookSecret, nil
}
func (store *fakeAppStore) ListGitHubInstallations(context.Context) ([]postgres.GitHubInstallation, error) {
	return []postgres.GitHubInstallation{{ID: 7, AccountLogin: "acme", AccountType: "Organization", Repositories: 2}}, nil
}
func (store *fakeAppStore) ApplyInstallationEvent(_ context.Context, _ string, _ string, _ []byte, event github.InstallationEvent) (bool, error) {
	store.installations = append(store.installations, event)
	return true, nil
}

func sign(body, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func TestGitHubManifestSetupFlow(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	t.Setenv("GITHUB_APP_ID", "")
	t.Setenv("GITHUB_WEBHOOK_SECRET", "")
	t.Setenv("OVERLOAD_BASE_URL", "http://example.com")
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app-manifests/one-time/conversions" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":42,"slug":"overload-test","html_url":"https://github.com/apps/overload-test","pem":"pem","webhook_secret":"hook-secret"}`))
	}))
	defer apiServer.Close()
	githubAPIBase = apiServer.URL
	t.Cleanup(func() { githubAPIBase = "" })

	store := &fakeAppStore{}
	handler := Handler(store)
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/setup/github", nil))
	match := csrfPattern.FindStringSubmatch(page.Body.String())
	if page.Code != http.StatusOK || match == nil {
		t.Fatalf("setup page %d", page.Code)
	}
	csrf := match[1]

	form := url.Values{"csrf": {csrf}, "name": {"overload-test"}, "webhook_url": {"https://hooks.example.com/webhooks/github"}}
	request := httptest.NewRequest(http.MethodPost, "/setup/github", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	stateMatch := regexp.MustCompile(`action="https://github.com/settings/apps/new\?state=([0-9a-f]+)"`).FindStringSubmatch(body)
	if response.Code != http.StatusOK || stateMatch == nil || !strings.Contains(body, "hooks.example.com") {
		t.Fatalf("manifest redirect %d %s", response.Code, body)
	}
	state := stateMatch[1]
	if state == csrf {
		t.Fatal("manifest state reuses the form token, which would leak it to GitHub")
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action https://github.com") {
		t.Fatal("manifest page CSP does not allow posting to GitHub")
	}

	form.Set("csrf", "wrong")
	request = httptest.NewRequest(http.MethodPost, "/setup/github", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("bad csrf accepted: %d", response.Code)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/setup/github/callback?code=one-time&state=wrong", nil))
	if response.Code != http.StatusForbidden || store.app != nil {
		t.Fatalf("callback without state: %d", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/setup/github/callback?code=one-time&state="+state, nil))
	if response.Code != http.StatusSeeOther || store.app == nil || store.app.AppID != 42 {
		t.Fatalf("callback %d %+v", response.Code, store.app)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/setup/github/callback?code=one-time&state="+state, nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("replayed callback state accepted: %d", response.Code)
	}
	page = httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/setup/github", nil))
	if !strings.Contains(page.Body.String(), "https://github.com/apps/overload-test/installations/new") || !strings.Contains(page.Body.String(), "acme") || strings.Contains(page.Body.String(), "hook-secret") {
		t.Fatal("configured page missing install link or leaks secret")
	}

	installation := `{"action":"created","installation":{"id":7,"account":{"login":"acme","type":"Organization"}},"repositories":[{"id":3,"full_name":"acme/api"}]}`
	for _, test := range []struct {
		signature string
		status    int
	}{{sign(installation, "other"), http.StatusUnauthorized}, {sign(installation, "hook-secret"), http.StatusAccepted}} {
		request = httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(installation))
		request.Header.Set("X-Hub-Signature-256", test.signature)
		request.Header.Set("X-GitHub-Delivery", "d1")
		request.Header.Set("X-GitHub-Event", "installation")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("installation webhook %d, want %d", response.Code, test.status)
		}
	}
	if len(store.installations) != 1 || store.installations[0].Repositories[0].FullName != "acme/api" {
		t.Fatalf("installation not applied: %+v", store.installations)
	}
}
