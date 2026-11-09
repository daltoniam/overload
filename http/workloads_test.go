package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/queue"
	"github.com/jackc/pgx/v5"
)

type fakeWorkloadStore struct {
	fakeKeyStore
	rules  []overload.WorkloadRule
	queued []string
}

func (store *fakeWorkloadStore) ListWorkloadRules(context.Context) ([]overload.WorkloadRule, error) {
	return store.rules, nil
}
func (store *fakeWorkloadStore) SaveWorkloadRule(_ context.Context, rule overload.WorkloadRule) error {
	for index := range store.rules {
		if store.rules[index].Name == rule.Name {
			store.rules[index] = rule
			return nil
		}
	}
	store.rules = append(store.rules, rule)
	return nil
}
func (store *fakeWorkloadStore) DeleteWorkloadRule(_ context.Context, name string) error {
	for index := range store.rules {
		if store.rules[index].Name == name {
			store.rules = append(store.rules[:index], store.rules[index+1:]...)
			return nil
		}
	}
	return pgx.ErrNoRows
}
func (store *fakeWorkloadStore) QueueReviewByName(_ context.Context, repository string, number int, _ bool) (int64, error) {
	if repository == "acme/closed" {
		return 0, fmt.Errorf("%w: pull request is closed", queue.ErrReviewNotQueued)
	}
	store.queued = append(store.queued, fmt.Sprintf("%s#%d", repository, number))
	return int64(len(store.queued)), nil
}

func TestReviewAPIAndWorkloadRules(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	discovery := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "k", Algorithm: "RS256"}}}
	issuer := httptest.NewTLSServer(discovery)
	defer issuer.Close()
	discovery.SetIssuer(issuer.URL)
	workloadClient = issuer.Client()
	t.Cleanup(func() { workloadClient = nil })
	token := func(repository string) string {
		return oidctest.SignIDToken(key, "k", "RS256", fmt.Sprintf(`{"iss":%q,"aud":"http://example.com","sub":"repo:%s","repository":%q,"exp":%d}`, issuer.URL, repository, repository, time.Now().Add(time.Minute).Unix()))
	}
	store := &fakeWorkloadStore{fakeKeyStore: fakeKeyStore{keys: []overload.APIKey{{ID: 1, Prefix: "ovl_test", Scopes: []string{overload.ScopeReviews}}}}}
	handler := Handler(store)
	post := func(path, bearer, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/configure/workloads/new", nil))
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())[1]
	form := func(values url.Values) *httptest.ResponseRecorder {
		values.Set("csrf", csrf)
		request := httptest.NewRequest(http.MethodPost, "/configure/workloads", strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := form(url.Values{"name": {"ci"}, "issuer": {overload.GitHubActionsIssuer}, "audience": {"http://example.com"}, "conditions": {"ref=refs/heads/main"}, "scopes": {"reviews"}, "enabled": {"true"}}); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a GitHub rule without a repository condition: %d", response.Code)
	}
	if response := form(url.Values{"name": {"ci"}, "issuer": {issuer.URL}, "audience": {"http://example.com"}, "conditions": {"repository=acme/*\r\nnot-a-condition"}, "scopes": {"reviews"}}); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a malformed condition: %d", response.Code)
	}
	if response := form(url.Values{"name": {"ci"}, "issuer": {issuer.URL}, "audience": {"http://example.com"}, "conditions": {"repository=acme/*\r\n"}, "scopes": {"reviews"}, "enabled": {"true"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("save rule: %d %s", response.Code, response.Body.String())
	}
	if len(store.rules) != 1 || store.rules[0].Conditions["repository"] != "acme/*" {
		t.Fatalf("rules = %+v", store.rules)
	}

	for name, test := range map[string]struct {
		bearer, body string
		status       int
	}{
		"no credentials":   {"", `{"repository":"acme/api","pull_request":3}`, http.StatusUnauthorized},
		"bad body":         {"ovl_test_secret", `{"repository":"acme/api"}`, http.StatusBadRequest},
		"unknown field":    {"ovl_test_secret", `{"repository":"acme/api","pull_request":3,"approve":true}`, http.StatusBadRequest},
		"API key":          {"ovl_test_secret", `{"repository":"acme/api","pull_request":3}`, http.StatusAccepted},
		"not queued":       {"ovl_test_secret", `{"repository":"acme/closed","pull_request":3}`, http.StatusConflict},
		"own repository":   {token("acme/api"), `{"repository":"acme/api","pull_request":4}`, http.StatusAccepted},
		"other repository": {token("acme/api"), `{"repository":"acme/web","pull_request":4}`, http.StatusUnauthorized},
		"untrusted owner":  {token("evil/api"), `{"repository":"evil/api","pull_request":4}`, http.StatusUnauthorized},
	} {
		if response := post("/api/reviews", test.bearer, test.body); response.Code != test.status {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	if strings.Join(store.queued, ",") != "acme/api#3,acme/api#4" && strings.Join(store.queued, ",") != "acme/api#4,acme/api#3" {
		t.Fatalf("queued = %v", store.queued)
	}
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/reviews", nil))
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/reviews: %d", get.Code)
	}
	if response := post("/v1/models", token("acme/api"), ""); response.Code != http.StatusForbidden {
		t.Fatalf("a reviews-only workload called models: %d", response.Code)
	}
}

func TestWorkloadRuleValidate(t *testing.T) {
	valid := overload.WorkloadRule{Name: "ci", Issuer: overload.GitHubActionsIssuer, Audience: "https://overload.example.com", Conditions: map[string]string{"repository": "acme/*"}, Scopes: []string{overload.ScopeReviews}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*overload.WorkloadRule){
		"http issuer":   func(rule *overload.WorkloadRule) { rule.Issuer = "http://issuer" },
		"no audience":   func(rule *overload.WorkloadRule) { rule.Audience = "" },
		"no conditions": func(rule *overload.WorkloadRule) { rule.Conditions = nil },
		"only ref":      func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"ref": "refs/heads/main"} },
		"bad pattern":   func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository": "["} },
		"unknown scope": func(rule *overload.WorkloadRule) { rule.Scopes = []string{"admin"} },
		"no scopes":     func(rule *overload.WorkloadRule) { rule.Scopes = nil },
		"bad claim": func(rule *overload.WorkloadRule) {
			rule.Conditions = map[string]string{"repository": "a/b", "a b": "x"}
		},
	} {
		rule := valid
		change(&rule)
		if err := rule.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	if !valid.Matches(map[string]any{"repository": "acme/api"}) || valid.Matches(map[string]any{"repository": "acme/api/extra"}) || valid.Matches(map[string]any{}) {
		t.Fatal("Matches is wrong")
	}
}
