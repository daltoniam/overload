package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/queue"
	"github.com/jackc/pgx/v5"
)

// fakeRules stores workload rules in memory.
type fakeRules struct {
	mu      sync.Mutex
	rules   []overload.WorkloadRule
	listErr error
}

func (store *fakeRules) ListWorkloadRules(context.Context) ([]overload.WorkloadRule, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]overload.WorkloadRule(nil), store.rules...), store.listErr
}
func (store *fakeRules) CreateWorkloadRule(_ context.Context, rule overload.WorkloadRule) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, existing := range store.rules {
		if existing.Name == rule.Name {
			return overload.ErrWorkloadRuleExists
		}
	}
	store.rules = append(store.rules, rule)
	return nil
}
func (store *fakeRules) UpdateWorkloadRule(_ context.Context, rule overload.WorkloadRule) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for index := range store.rules {
		if store.rules[index].Name == rule.Name {
			store.rules[index] = rule
			return nil
		}
	}
	return pgx.ErrNoRows
}
func (store *fakeRules) DeleteWorkloadRule(_ context.Context, name string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for index := range store.rules {
		if store.rules[index].Name == name {
			store.rules = append(store.rules[:index], store.rules[index+1:]...)
			return nil
		}
	}
	return pgx.ErrNoRows
}
func (store *fakeRules) rule(name string) (overload.WorkloadRule, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, rule := range store.rules {
		if rule.Name == name {
			return rule, true
		}
	}
	return overload.WorkloadRule{}, false
}

type fakeWorkloadStore struct {
	fakeKeyStore
	fakeRules
	queued []string
}

func (store *fakeWorkloadStore) QueueReviewByName(_ context.Context, repository string, number int, _ bool) (int64, error) {
	switch repository {
	case "acme/closed":
		return 0, fmt.Errorf("%w: pull request is closed", queue.ErrReviewNotQueued)
	case "acme/broken":
		return 0, errors.New("dial tcp 10.0.0.9:5432: connection refused")
	}
	store.queued = append(store.queued, fmt.Sprintf("%s#%d", repository, number))
	return int64(len(store.queued)), nil
}

// testIssuer is a fake workload token issuer.
type testIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	discovery := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "k", Algorithm: "RS256"}}}
	server := httptest.NewTLSServer(discovery)
	t.Cleanup(server.Close)
	discovery.SetIssuer(server.URL)
	return &testIssuer{server: server, key: key}
}

func (issuer *testIssuer) token(subject, claims string) string {
	return oidctest.SignIDToken(issuer.key, "k", "RS256", fmt.Sprintf(`{"iss":%q,"aud":"http://example.com","sub":%q,"exp":%d%s}`, issuer.server.URL, subject, time.Now().Add(time.Minute).Unix(), claims))
}

func (issuer *testIssuer) repositoryToken(repository string) string {
	return issuer.token("repo:"+repository+":ref:refs/pull/4/merge", fmt.Sprintf(`,"repository":%q,"ref":"refs/pull/4/merge"`, repository))
}

func TestReviewAPIAndWorkloadRules(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	issuer := newTestIssuer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	store := &fakeWorkloadStore{fakeKeyStore: fakeKeyStore{
		fakeStore: fakeStore{settings: []overload.ReviewSettings{{Name: "flint", Provider: overload.ProviderOpenAICompat, BaseURL: upstream.URL + "/v1", Model: "flint", Concurrency: 1}}},
		keys:      []overload.APIKey{{ID: 1, Prefix: "ovl_test", Scopes: []string{overload.ScopeReviews}}},
	}}
	handler := Handler(store, WithWorkloadTransport(issuer.server.Client().Transport))
	call := func(method, path, authorization, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/configure/workloads/new", nil))
	if !strings.Contains(page.Body.String(), "refs/pull/*/merge") || strings.Contains(page.Body.String(), "ref=refs/heads/main") {
		t.Fatal("the condition placeholder suggests a branch ref, which pull request jobs never have")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())[1]
	form := func(path string, values url.Values) *httptest.ResponseRecorder {
		values.Set("csrf", csrf)
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	rule := func(name, issuerURL, conditions string) url.Values {
		return url.Values{"name": {name}, "issuer": {issuerURL}, "audience": {"http://example.com"}, "conditions": {conditions}, "scopes": {"reviews"}, "enabled": {"true"}}
	}
	for name, test := range map[string]struct {
		values url.Values
		status int
		says   string
	}{
		"GitHub rule without an owner": {rule("ci", overload.GitHubActionsIssuer, "ref=refs/heads/main"), http.StatusUnprocessableEntity, "must name one owner"},
		"GitHub any repository":        {rule("ci", overload.GitHubActionsIssuer, "repository=*/*"), http.StatusUnprocessableEntity, "must name one owner"},
		"GitHub any owner":             {rule("ci", overload.GitHubActionsIssuer, "repository=*/api"), http.StatusUnprocessableEntity, "must name one owner"},
		"GitHub wildcard owner claim":  {rule("ci", overload.GitHubActionsIssuer, "repository_owner=*"), http.StatusUnprocessableEntity, "must name one owner"},
		"enterprise issuer, no owner":  {rule("ci", overload.GitHubActionsIssuer+"/acme", "repository=a?me/api"), http.StatusUnprocessableEntity, "must name one owner"},
		"malformed condition":          {rule("ci", issuer.server.URL, "repository=acme/*\r\nnot-a-condition"), http.StatusUnprocessableEntity, "claim=pattern"},
		"reserved name":                {rule("new", issuer.server.URL, "repository=acme/*"), http.StatusUnprocessableEntity, "reserved"},
	} {
		response := form("/configure/workloads", test.values)
		body := response.Body.String()
		if response.Code != test.status || !strings.Contains(body, test.says) || !strings.Contains(body, `id="form-errors"`) || !strings.Contains(body, `value="`+test.values.Get("name")+`"`) || !strings.Contains(body, test.values.Get("conditions")[:5]) {
			t.Errorf("%s: %d, input not kept or error not shown: %s", name, response.Code, body)
		}
	}
	if response := form("/configure/workloads", rule("ci", issuer.server.URL, "repository=acme/*\r\n")); response.Code != http.StatusSeeOther {
		t.Fatalf("save rule: %d %s", response.Code, response.Body.String())
	}
	if saved, _ := store.rule("ci"); saved.Conditions["repository"] != "acme/*" {
		t.Fatalf("rules = %+v", store.rules)
	}
	if response := form("/configure/workloads", rule("ci", issuer.server.URL, "sub=anything")); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "already exists") || !strings.Contains(response.Body.String(), "sub=anything") {
		t.Fatalf("a new rule reusing a name: %d", response.Code)
	}
	if saved, _ := store.rule("ci"); saved.Conditions["repository"] != "acme/*" {
		t.Fatalf("a new rule overwrote an existing one: %+v", saved)
	}
	k8s := rule("k8s", issuer.server.URL, "sub=system:serviceaccount:ci:*")
	k8s["scopes"] = []string{"reviews", "models"}
	if response := form("/configure/workloads", k8s); response.Code != http.StatusSeeOther {
		t.Fatalf("save the Kubernetes rule: %d %s", response.Code, response.Body.String())
	}

	k8sToken := issuer.token("system:serviceaccount:ci:runner", "")
	for name, test := range map[string]struct {
		authorization, body string
		status              int
	}{
		"no credentials":              {"", `{"repository":"acme/api","pull_request":3}`, http.StatusUnauthorized},
		"no credentials, bad body":    {"", `nonsense`, http.StatusUnauthorized},
		"unknown credential":          {"Bearer nonsense", `{"repository":"acme/api","pull_request":3}`, http.StatusUnauthorized},
		"bad body":                    {"Bearer ovl_test_secret", `{"repository":"acme/api"}`, http.StatusBadRequest},
		"unknown field":               {"Bearer ovl_test_secret", `{"repository":"acme/api","pull_request":3,"approve":true}`, http.StatusBadRequest},
		"API key":                     {"Bearer ovl_test_secret", `{"repository":"acme/api","pull_request":3}`, http.StatusAccepted},
		"lowercase scheme":            {"bearer ovl_test_secret", `{"repository":"acme/api","pull_request":5}`, http.StatusAccepted},
		"revoked or wrong key":        {"Bearer ovl_wrong", `{"repository":"acme/api","pull_request":3}`, http.StatusUnauthorized},
		"not queued":                  {"Bearer ovl_test_secret", `{"repository":"acme/closed","pull_request":3}`, http.StatusConflict},
		"queue failure":               {"Bearer ovl_test_secret", `{"repository":"acme/broken","pull_request":3}`, http.StatusServiceUnavailable},
		"own repository":              {"Bearer " + issuer.repositoryToken("acme/api"), `{"repository":"acme/api","pull_request":4}`, http.StatusAccepted},
		"other repository":            {"Bearer " + issuer.repositoryToken("acme/api"), `{"repository":"acme/web","pull_request":4}`, http.StatusForbidden},
		"untrusted owner":             {"Bearer " + issuer.repositoryToken("evil/api"), `{"repository":"evil/api","pull_request":4}`, http.StatusUnauthorized},
		"no repository claim reviews": {"Bearer " + k8sToken, `{"repository":"acme/web","pull_request":6}`, http.StatusAccepted},
	} {
		response := call(http.MethodPost, "/api/reviews", test.authorization, test.body)
		if response.Code != test.status {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
		if (response.Code == http.StatusUnauthorized) != (response.Header().Get("WWW-Authenticate") == "Bearer") {
			t.Errorf("%s: WWW-Authenticate %q with %d", name, response.Header().Get("WWW-Authenticate"), response.Code)
		}
		if strings.Contains(response.Body.String(), "10.0.0.9") {
			t.Errorf("%s: leaked an internal error: %s", name, response.Body.String())
		}
	}
	if got := strings.Join(store.queued, ","); len(store.queued) != 4 || !strings.Contains(got, "acme/api#4") || !strings.Contains(got, "acme/web#6") {
		t.Fatalf("queued = %v", store.queued)
	}
	if get := call(http.MethodGet, "/api/reviews", "", ""); get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/reviews: %d", get.Code)
	}

	again := `{"repository":"acme/api","pull_request":4,"again":true}`
	for range rerunsPerHour {
		if response := call(http.MethodPost, "/api/reviews", "Bearer "+issuer.repositoryToken("acme/api"), again); response.Code != http.StatusAccepted {
			t.Fatalf("forced re-review: %d", response.Code)
		}
	}
	if response := call(http.MethodPost, "/api/reviews", "Bearer "+issuer.repositoryToken("acme/api"), again); response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("forced re-review past the limit: %d", response.Code)
	}
	if response := call(http.MethodPost, "/api/reviews", "Bearer "+issuer.repositoryToken("acme/api"), `{"repository":"acme/api","pull_request":4}`); response.Code != http.StatusAccepted {
		t.Fatalf("an ordinary review after the re-review limit: %d", response.Code)
	}

	if response := call(http.MethodGet, "/v1/models", "Bearer "+issuer.repositoryToken("acme/api"), ""); response.Code != http.StatusForbidden {
		t.Fatalf("a reviews-only workload called models: %d", response.Code)
	}
	if response := call(http.MethodGet, "/v1/models", "Bearer "+k8sToken, ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "flint") {
		t.Fatalf("a models workload listed models: %d %s", response.Code, response.Body.String())
	}
	if response := call(http.MethodPost, "/v1/chat/completions", "Bearer "+k8sToken, `{"model":"flint"}`); response.Code != http.StatusOK || len(store.usage) != 1 || store.usage[0].Workload != "k8s" || store.usage[0].KeyID != 0 || store.usage[0].InputTokens != 3 {
		t.Fatalf("a models workload called a model: %d %+v", response.Code, store.usage)
	}

	confirm := form("/configure/workloads/k8s/delete", url.Values{})
	if _, kept := store.rule("k8s"); !kept || confirm.Code != http.StatusOK || !strings.Contains(confirm.Body.String(), "Delete workload rule k8s?") || !strings.Contains(confirm.Body.String(), `name="confirmed" value="true"`) {
		t.Fatalf("delete without confirmation: %d %s", confirm.Code, confirm.Body.String())
	}
	if response := form("/configure/workloads/k8s/delete", url.Values{"confirmed": {"true"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", response.Code)
	}
	if response := call(http.MethodGet, "/v1/models", "Bearer "+k8sToken, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("a deleted rule still trusted its tokens: %d", response.Code)
	}

	store.mu.Lock()
	store.listErr = errors.New("dial tcp 10.0.0.9:5432: connection refused")
	store.mu.Unlock()
	form("/configure/workloads/ci/delete", url.Values{"confirmed": {"true"}})
	for _, path := range []string{"/api/reviews", "/v1/models"} {
		response := call(http.MethodPost, path, "Bearer "+issuer.repositoryToken("acme/api"), `{"repository":"acme/api","pull_request":4}`)
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "10.0.0.9") {
			t.Errorf("%s with unreadable rules: %d %s", path, response.Code, response.Body.String())
		}
	}
}

func TestRerunLimiter(t *testing.T) {
	now := time.Now()
	limiter := newRerunLimiter(2, time.Hour)
	limiter.now = func() time.Time { return now }
	for _, test := range []struct {
		caller  string
		advance time.Duration
		ok      bool
	}{
		{"workload:ci acme/api", 0, true},
		{"workload:ci acme/api", time.Minute, true},
		{"workload:ci acme/api", time.Minute, false},
		{"workload:ci acme/web", 0, true},
		{"workload:ci acme/api", time.Hour - time.Minute, true},
	} {
		now = now.Add(test.advance)
		if _, ok := limiter.allow(test.caller); ok != test.ok {
			t.Errorf("%s after %s: allowed %v", test.caller, test.advance, ok)
		}
	}
}

type oidcWorkloadStore struct {
	*fakeSessionStore
	*fakeRules
}

func TestWorkloadRulesWithOIDCSessions(t *testing.T) {
	provider := newFakeProvider(t)
	oidcEnv(t, provider.server.URL)
	if err := ValidateAuth(); err != nil {
		t.Fatal(err)
	}
	store := oidcWorkloadStore{newFakeSessionStore(), &fakeRules{}}
	handler := Handler(store)
	admin := newBrowser(t, handler)
	provider.signsIn("ada", "platform")
	admin.signIn(provider, "/")
	token := admin.formToken("/")
	// formToken reads the csrf token of the form posting to
	// action; the sign-out form also carries a token, so look at this one.
	formToken := func(page, action string) string {
		match := regexp.MustCompile(`action="` + regexp.QuoteMeta(action) + `"[^>]*><input type="hidden" name="csrf" value="([a-f0-9]*)"`).FindStringSubmatch(page)
		if match == nil {
			t.Fatalf("no form posting to %s", action)
		}
		return match[1]
	}
	if got := formToken(admin.do(http.MethodGet, "/configure/workloads/new", nil).Body.String(), "/configure/workloads"); got != token {
		t.Fatalf("the new rule form carries token %q, not the person's own", got)
	}
	values := url.Values{"csrf": {token}, "name": {"ci"}, "issuer": {overload.GitHubActionsIssuer}, "audience": {"https://overload.example.com"}, "conditions": {"repository=acme/api"}, "scopes": {"reviews"}, "enabled": {"true"}}
	if response := admin.do(http.MethodPost, "/configure/workloads", values); response.Code != http.StatusSeeOther {
		t.Fatalf("save a rule while signed in: %d %s", response.Code, response.Body.String())
	}
	edit := admin.do(http.MethodGet, "/configure/workloads/ci", nil).Body.String()
	if formToken(edit, "/configure/workloads") != token || formToken(edit, "/configure/workloads/ci/delete") != token || !strings.Contains(edit, `name="existing" value="ci"`) {
		t.Fatal("the edit form does not carry the person's own form token")
	}
	values.Set("existing", "ci")
	values.Set("conditions", "repository=acme/api\nenvironment=review")
	if response := admin.do(http.MethodPost, "/configure/workloads", values); response.Code != http.StatusSeeOther {
		t.Fatalf("edit a rule while signed in: %d", response.Code)
	}
	if saved, _ := store.rule("ci"); saved.Conditions["environment"] != "review" {
		t.Fatalf("rule not updated: %+v", saved)
	}
	values.Set("conditions", "repository=*/*")
	if response := admin.do(http.MethodPost, "/configure/workloads", values); response.Code != http.StatusUnprocessableEntity || formToken(response.Body.String(), "/configure/workloads") != token || !strings.Contains(response.Body.String(), "repository=*/*") {
		t.Fatalf("a failed edit while signed in: %d", response.Code)
	}
	prompt := admin.do(http.MethodPost, "/configure/workloads/ci/delete", url.Values{"csrf": {token}})
	if prompt.Code != http.StatusOK || !strings.Contains(prompt.Body.String(), `name="csrf" value="`+token+`"`) {
		t.Fatalf("delete prompt: %d", prompt.Code)
	}
	if response := admin.do(http.MethodPost, "/configure/workloads/ci/delete", url.Values{"csrf": {token}, "confirmed": {"true"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("delete a rule while signed in: %d", response.Code)
	}
	if _, kept := store.rule("ci"); kept {
		t.Fatal("rule not deleted")
	}
}

func TestWorkloadRuleValidate(t *testing.T) {
	valid := overload.WorkloadRule{Name: "ci", Issuer: overload.GitHubActionsIssuer, Audience: "https://overload.example.com", Conditions: map[string]string{"repository": "acme/*"}, Scopes: []string{overload.ScopeReviews}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		change func(*overload.WorkloadRule)
		valid  bool
	}{
		"http issuer":       {func(rule *overload.WorkloadRule) { rule.Issuer = "http://issuer" }, false},
		"no audience":       {func(rule *overload.WorkloadRule) { rule.Audience = "" }, false},
		"no conditions":     {func(rule *overload.WorkloadRule) { rule.Conditions = nil }, false},
		"only ref":          {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"ref": "refs/heads/main"} }, false},
		"bad pattern":       {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository": "["} }, false},
		"unknown scope":     {func(rule *overload.WorkloadRule) { rule.Scopes = []string{"admin"} }, false},
		"no scopes":         {func(rule *overload.WorkloadRule) { rule.Scopes = nil }, false},
		"reserved name":     {func(rule *overload.WorkloadRule) { rule.Name = "new" }, false},
		"any repository":    {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository": "*/*"} }, false},
		"any owner":         {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository": "*/api"} }, false},
		"wildcard owner":    {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository_owner": "*"} }, false},
		"escaped owner":     {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository_owner": `ac\me`} }, false},
		"wildcard owner id": {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository_owner_id": "12*"} }, false},
		"enterprise issuer": {func(rule *overload.WorkloadRule) {
			rule.Issuer += "/acme"
			rule.Conditions = map[string]string{"sub": "*"}
		}, false},
		"literal owner": {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository_owner": "acme"} }, true},
		"owner id":      {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository_owner_id": "1234"} }, true},
		"repository id": {func(rule *overload.WorkloadRule) { rule.Conditions = map[string]string{"repository_id": "99"} }, true},
		"other issuer, any": {func(rule *overload.WorkloadRule) {
			rule.Issuer = "https://k8s.example.com"
			rule.Conditions = map[string]string{"sub": "*"}
		}, true},
		"enterprise, literal": {func(rule *overload.WorkloadRule) { rule.Issuer += "/acme" }, true},
		"bad claim": {func(rule *overload.WorkloadRule) {
			rule.Conditions = map[string]string{"repository": "a/b", "a b": "x"}
		}, false},
	} {
		rule := valid
		test.change(&rule)
		if err := rule.Validate(); (err == nil) != test.valid {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !valid.Matches(map[string]any{"repository": "acme/api"}) || valid.Matches(map[string]any{"repository": "acme/api/extra"}) || valid.Matches(map[string]any{}) {
		t.Fatal("Matches is wrong")
	}
}
