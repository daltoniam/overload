package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/postgres"
)

type fakePolicyStore struct {
	fakeStore
	policy catalog.Policy
}

func (store *fakePolicyStore) LoadModelPolicy(context.Context) (catalog.Policy, error) {
	return store.policy, nil
}

func (store *fakePolicyStore) SaveModelPolicy(_ context.Context, policy catalog.Policy) error {
	store.policy = policy
	return nil
}

func (store *fakePolicyStore) SaveReviewSettings(ctx context.Context, setting overload.ReviewSettings) error {
	if err := store.policy.Check(setting.Catalog, setting.Model).Error(); err != nil {
		return fmt.Errorf("%w: %v", postgres.ErrModelPolicy, err)
	}
	return store.fakeStore.SaveReviewSettings(ctx, setting)
}

func TestCatalogAndPolicyPages(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &fakePolicyStore{fakeStore: fakeStore{settings: []overload.ReviewSettings{{Name: "qwen", Provider: "openaicompat", ConnectionKind: "local", BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen3.8-flash-next", PromptProfile: "context", Catalog: "qwen3.8-flash-next"}}}}
	handler := Handler(store)
	get := func(path string, status int) string {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != status {
			t.Fatalf("%s: status %d, want %d", path, response.Code, status)
		}
		return response.Body.String()
	}
	post := func(path string, form url.Values, status int) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s: status %d, want %d: %s", path, response.Code, status, response.Body.String())
		}
		return response.Body.String()
	}

	listing := get("/catalog", http.StatusOK)
	if !strings.Contains(listing, `href="/catalog/recursive-flint"`) || !strings.Contains(listing, "every model is allowed") || strings.Contains(listing, "Refused") {
		t.Fatal("catalog page wrong without a policy")
	}
	entry := get("/catalog/recursive-flint", http.StatusOK)
	if !strings.Contains(entry, "b11801") || !strings.Contains(entry, "Trained from scratch") || !strings.Contains(entry, "15 of 20") {
		t.Fatal("catalog entry page misses provenance, runtime or eval")
	}
	get("/catalog/missing", http.StatusNotFound)
	if !strings.Contains(get("/settings/new", http.StatusOK), `value="recursive-flint"`) {
		t.Fatal("model form has no catalog choice")
	}

	form := get("/catalog/policy", http.StatusOK)
	match := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(form)
	if len(match) != 2 {
		t.Fatal("missing policy form token")
	}
	csrf := match[1]
	retained := post("/catalog/policy", url.Values{"csrf": {csrf}, "allowed_origins": {"US"}, "blocked_origins": {"US"}, "check_lineage": {"true"}, "licence_reviewed": {"twil-8b"}}, http.StatusUnprocessableEntity)
	if !strings.Contains(retained, "US is both allowed and blocked") || !strings.Contains(retained, `value="US"`) || !regexp.MustCompile(`value="twil-8b" checked`).MatchString(retained) || !regexp.MustCompile(`name="check_lineage" value="true" checked`).MatchString(retained) {
		t.Fatalf("invalid policy form was not shown again with its values: %s", retained)
	}
	post("/catalog/policy", url.Values{"csrf": {csrf}, "allowed_origins": {"US"}, "annual_revenue_usd": {"lots"}}, http.StatusUnprocessableEntity)
	post("/catalog/policy", url.Values{"csrf": {csrf}, "allowed_origins": {"us, ca"}, "blocked_origins": {"cn"}, "check_lineage": {"true"}, "require_commercial_use": {"true"}, "licence_reviewed": {"twil-8b"}, "lineage_reviewed": {"gemma-4-31b"}, "annual_revenue_usd": {"5000000"}}, http.StatusSeeOther)
	if strings.Join(store.policy.AllowedOrigins, ",") != "CA,US" || !store.policy.CheckLineage || store.policy.AnnualRevenueUSD != 5_000_000 || len(store.policy.LicenceReviewed) != 1 || len(store.policy.LineageReviewed) != 1 {
		t.Fatalf("saved policy = %+v", store.policy)
	}
	if !strings.Contains(get("/catalog/policy", http.StatusOK), "qwen: its publisher is from CN") {
		t.Fatal("policy page does not list the refused model")
	}
	models := get("/settings", http.StatusOK)
	if !strings.Contains(models, "Refused by policy") || !strings.Contains(models, "Qwen3.8 Flash Next") {
		t.Fatal("models list does not flag the refused model")
	}
	body := post("/settings", url.Values{"csrf": {csrf}, "name": {"other"}, "connection_kind": {"local"}, "model": {"deepseek"}, "base_url": {"http://127.0.0.1:8000/v1"}, "catalog": {"deepseek-v4-flash"}}, http.StatusConflict)
	if !strings.Contains(body, "Model not saved: its publisher is from CN") {
		t.Fatalf("refusal message = %q", body)
	}
	post("/settings", url.Values{"csrf": {csrf}, "name": {"flint"}, "connection_kind": {"local"}, "model": {"flint"}, "base_url": {"http://127.0.0.1:8080/v1"}, "catalog": {"recursive-flint"}}, http.StatusSeeOther)
	post("/settings", url.Values{"csrf": {csrf}, "name": {"bogus"}, "connection_kind": {"local"}, "model": {"x"}, "base_url": {"http://127.0.0.1:8080/v1"}, "catalog": {"not-in-catalog"}}, http.StatusBadRequest)
}
