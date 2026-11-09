package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/jackc/pgx/v5"
)

type fakeKeyStore struct {
	fakeStore
	keys  []overload.APIKey
	usage []overload.Usage
}

func (store *fakeKeyStore) CreateAPIKey(_ context.Context, userID int64, name string, scopes []string, lifetime time.Duration) (overload.APIKey, string, error) {
	key := overload.APIKey{ID: int64(len(store.keys) + 1), UserID: userID, Name: name, Prefix: "ovl_test", Scopes: scopes, ExpiresAt: time.Now().Add(lifetime)}
	store.keys = append(store.keys, key)
	return key, "ovl_test_secret", nil
}
func (store *fakeKeyStore) ListAPIKeys(context.Context, int64, bool) ([]overload.APIKey, error) {
	return store.keys, nil
}
func (store *fakeKeyStore) RevokeAPIKey(_ context.Context, id, _ int64, _ bool) error {
	if id < 1 || int(id) > len(store.keys) {
		return pgx.ErrNoRows
	}
	now := time.Now()
	store.keys[id-1].RevokedAt = &now
	return nil
}
func (store *fakeKeyStore) UsageTotals(context.Context, time.Time, int64, bool) ([]overload.UsageTotal, error) {
	return []overload.UsageTotal{{UserEmail: "a@example.com", Model: "flint", Calls: 2, InputTokens: 10, OutputTokens: 4, Minutes: 1.5}}, nil
}
func (store *fakeKeyStore) AuthenticateAPIKey(_ context.Context, secret string) (overload.APIKey, *overload.User, error) {
	if secret != "ovl_test_secret" || store.keys[0].RevokedAt != nil {
		return overload.APIKey{}, nil, pgx.ErrNoRows
	}
	return store.keys[0], nil, nil
}
func (store *fakeKeyStore) LoadModelPolicy(context.Context) (catalog.Policy, error) {
	return catalog.Policy{}, nil
}
func (store *fakeKeyStore) RecordUsage(_ context.Context, usage overload.Usage) error {
	store.usage = append(store.usage, usage)
	return nil
}

func TestKeysPagesAndGatewayRoute(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	store := &fakeKeyStore{fakeStore: fakeStore{settings: []overload.ReviewSettings{{Name: "flint", Provider: overload.ProviderOpenAICompat, BaseURL: upstream.URL + "/v1", Model: "flint", Concurrency: 1}}}}
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
	post := func(path string, form url.Values, status int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		return response
	}
	page := get("/keys")
	if !strings.Contains(page, "No API keys yet") || !strings.Contains(page, `action="/keys"`) {
		t.Fatal("keys page wrong")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page)[1]
	post("/keys", url.Values{"csrf": {csrf}, "name": {"laptop"}, "scopes": {"models"}, "days": {"400"}}, http.StatusUnprocessableEntity)
	post("/keys", url.Values{"csrf": {csrf}, "name": {"laptop"}, "days": {"30"}}, http.StatusUnprocessableEntity)
	created := post("/keys", url.Values{"csrf": {csrf}, "name": {"laptop"}, "scopes": {"models"}, "days": {"30"}}, http.StatusOK)
	if !strings.Contains(created.Body.String(), "ovl_test_secret") || created.Header().Get("Cache-Control") != "no-store" || !strings.Contains(created.Body.String(), "http://example.com/v1") {
		t.Fatalf("created page: %v %s", created.Header(), created.Body.String())
	}
	if strings.Contains(get("/keys"), "ovl_test_secret") {
		t.Fatal("the secret is shown again")
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"flint"}`))
	request.Header.Set("Authorization", "Bearer ovl_test_secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(store.usage) != 1 {
		t.Fatalf("gateway through the server: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Host = "attacker.test"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("gateway on an unknown host: %d", response.Code)
	}

	post("/keys/1/revoke", url.Values{"csrf": {csrf}}, http.StatusSeeOther)
	post("/keys/9/revoke", url.Values{"csrf": {csrf}}, http.StatusNotFound)
	if !strings.Contains(get("/keys"), "Revoked") {
		t.Fatal("revoked key not shown as revoked")
	}
	if usage := get("/usage"); !strings.Contains(usage, "a@example.com") || !strings.Contains(usage, "1.5") {
		t.Fatal("usage page wrong")
	}
	if allowed(overload.RoleViewer, httptest.NewRequest(http.MethodPost, "/keys", nil)) || !allowed(overload.RoleMember, httptest.NewRequest(http.MethodPost, "/keys/3/revoke", nil)) {
		t.Fatal("key roles are wrong")
	}
}
