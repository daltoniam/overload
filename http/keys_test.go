package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
)

type fakeKeyStore struct {
	fakeStore
	listErr error
	keys    []overload.APIKey
	usage   []overload.Usage
}

func (store *fakeKeyStore) CreateAPIKey(_ context.Context, userID int64, name string, scopes []string, lifetime time.Duration) (overload.APIKey, string, error) {
	key := overload.APIKey{ID: int64(len(store.keys) + 1), UserID: userID, Name: name, Prefix: "ovl_test", Scopes: scopes, ExpiresAt: time.Now().Add(lifetime)}
	store.keys = append(store.keys, key)
	return key, "ovl_test_secret", nil
}
func (store *fakeKeyStore) ListAPIKeys(context.Context, int64, bool) ([]overload.APIKey, error) {
	return store.keys, store.listErr
}
func (store *fakeKeyStore) RevokeAPIKey(_ context.Context, id, _ int64, _ bool) error {
	if id < 1 || int(id) > len(store.keys) {
		return overload.ErrInvalidAPIKey
	}
	now := time.Now()
	store.keys[id-1].RevokedAt = &now
	return nil
}
func (store *fakeKeyStore) GetAPIKey(_ context.Context, id, _ int64, _ bool) (overload.APIKey, error) {
	if id < 1 || int(id) > len(store.keys) {
		return overload.APIKey{}, overload.ErrInvalidAPIKey
	}
	return store.keys[id-1], nil
}
func (store *fakeKeyStore) UsageTotals(context.Context, time.Time, int64, bool) ([]overload.UsageTotal, error) {
	return []overload.UsageTotal{{UserEmail: "a@example.com", Model: "flint", Calls: 2, InputTokens: 10, OutputTokens: 4, Minutes: 1.5}}, nil
}
func (store *fakeKeyStore) AuthenticateAPIKey(_ context.Context, secret string) (overload.APIKey, *overload.User, error) {
	if secret != "ovl_test_secret" || store.keys[0].RevokedAt != nil {
		return overload.APIKey{}, nil, overload.ErrInvalidAPIKey
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
	createForm := regexp.MustCompile(`<form[^>]*action="/keys"[^>]*>`).FindString(page)
	if !strings.Contains(createForm, `data-full-page="true"`) || strings.Contains(createForm, "data-enhance") {
		t.Fatalf("the create form would be swapped into the error area by HTMX: %s", createForm)
	}
	if !strings.Contains(page, "No API keys yet") || !strings.Contains(page, `action="/keys"`) {
		t.Fatal("keys page wrong")
	}
	if strings.Contains(page, `value="reviews"`) {
		t.Fatal("offered the reviews scope, which nothing accepts yet")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page)[1]
	for _, form := range []url.Values{
		{"csrf": {csrf}, "name": {"my laptop"}, "scopes": {"models"}, "days": {"400"}},
		{"csrf": {csrf}, "name": {"my laptop"}, "days": {"30"}},
		{"csrf": {csrf}, "name": {"my laptop"}, "scopes": {"reviews"}, "days": {"30"}},
	} {
		retained := post("/keys", form, http.StatusUnprocessableEntity).Body.String()
		if !strings.Contains(retained, `value="my laptop"`) || !strings.Contains(retained, `value="`+form.Get("days")+`"`) || !strings.Contains(retained, "Key not created") || !strings.Contains(retained, `id="form-errors"`) {
			t.Fatalf("validation lost the form: %s", retained)
		}
	}
	created := post("/keys", url.Values{"csrf": {csrf}, "name": {"laptop"}, "scopes": {"models"}, "days": {"30"}}, http.StatusOK)
	if !strings.Contains(created.Body.String(), "ovl_test_secret") || created.Header().Get("Cache-Control") != "no-store" || !strings.Contains(created.Body.String(), "http://example.com/v1") {
		t.Fatalf("created page: %v %s", created.Header(), created.Body.String())
	}
	store.listErr = errors.New("database unavailable")
	if again := post("/keys", url.Values{"csrf": {csrf}, "name": {"ci"}, "scopes": {"models"}, "days": {"30"}}, http.StatusOK); !strings.Contains(again.Body.String(), "ovl_test_secret") || !strings.Contains(again.Body.String(), "Your other keys could not be loaded") {
		t.Fatal("the new key's secret was lost when the list failed to load")
	}
	store.listErr, store.keys = nil, store.keys[:1]
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
	request.Host = "overload.overload.svc:8082"
	request.Header.Set("Authorization", "Bearer ovl_test_secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway by its in-cluster name: %d", response.Code)
	}
	if page := get("/keys"); !strings.Contains(page, `data-confirm="Revoke &#34;laptop&#34; (ovl_test)?`) || strings.Contains(page, `action="/keys/1/revoke" data-enhance`) {
		t.Fatalf("revoke form has no confirmation: %s", page)
	}

	confirm := post("/keys/1/revoke", url.Values{"csrf": {csrf}}, http.StatusOK).Body.String()
	if store.keys[0].RevokedAt != nil || !strings.Contains(confirm, "Revoke API key laptop?") || !strings.Contains(confirm, "stop working at once") || !strings.Contains(confirm, `name="confirmed" value="true"`) || !strings.Contains(confirm, `href="/keys"`) {
		t.Fatalf("unconfirmed revoke: %v %s", store.keys[0].RevokedAt, confirm)
	}
	post("/keys/9/revoke", url.Values{"csrf": {csrf}}, http.StatusNotFound)
	post("/keys/1/revoke", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusSeeOther)
	post("/keys/9/revoke", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusNotFound)
	post("/keys/1/revoke", url.Values{"csrf": {csrf}}, http.StatusNotFound)
	if !strings.Contains(get("/keys"), "Revoked") {
		t.Fatal("revoked key not shown as revoked")
	}
	store.keys = append(store.keys, overload.APIKey{ID: 2, Name: "old", Prefix: "ovl_old", Scopes: []string{overload.ScopeModels}, ExpiresAt: time.Now().Add(-time.Hour)})
	if page := get("/keys"); !strings.Contains(page, `<span class="badge disabled">Expired</span>`) || strings.Contains(page, "/keys/2/revoke") || strings.Contains(page, "newest keys") {
		t.Fatalf("expired key: %s", page)
	}
	post("/keys/2/revoke", url.Values{"csrf": {csrf}}, http.StatusNotFound)
	for len(store.keys) < overload.APIKeyListLimit {
		store.keys = append(store.keys, overload.APIKey{ID: int64(len(store.keys) + 1), Name: "bulk", ExpiresAt: time.Now().Add(time.Hour)})
	}
	if !strings.Contains(get("/keys"), "Showing the 500 newest keys.") {
		t.Fatal("capped list not marked")
	}
	if usage := get("/usage"); !strings.Contains(usage, "a@example.com") || !strings.Contains(usage, "1.5") {
		t.Fatal("usage page wrong")
	}
	if allowed(overload.RoleViewer, httptest.NewRequest(http.MethodPost, "/keys", nil)) || !allowed(overload.RoleMember, httptest.NewRequest(http.MethodPost, "/keys/3/revoke", nil)) {
		t.Fatal("key roles are wrong")
	}
}

func TestGatewayBase(t *testing.T) {
	for _, test := range []struct{ base, addr, want string }{
		{"https://overload.example.com/", "", "https://overload.example.com"},
		{"", "", "http://127.0.0.1:8082"},
		{"", "0.0.0.0:9000", "http://127.0.0.1:9000"},
		{"", "[::]:9000", "http://127.0.0.1:9000"},
		{"", "192.168.1.5:9000", "http://192.168.1.5:9000"},
		{"", "nonsense", "http://127.0.0.1:8082"},
	} {
		t.Setenv("OVERLOAD_BASE_URL", test.base)
		t.Setenv("OVERLOAD_ADDR", test.addr)
		if got := gatewayBase(); got != test.want {
			t.Errorf("%q %q: %s", test.base, test.addr, got)
		}
	}
}
