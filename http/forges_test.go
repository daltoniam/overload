package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/forge"
	"github.com/daltoniam/overload/postgres"
)

type forgeStore struct {
	fakeStore
	connections []overload.ForgeConnection
	repos       []overload.Repository
	ingested    []forge.Event
}

func (store *forgeStore) ListForges(context.Context) ([]overload.ForgeConnection, error) {
	return store.connections, nil
}

func (store *forgeStore) SaveForge(_ context.Context, connection overload.ForgeConnection) (overload.ForgeConnection, error) {
	connection.ID = int64(len(store.connections) + 1)
	connection.WebhookSecret = "generated-secret"
	store.connections = append(store.connections, connection)
	return connection, nil
}

func (store *forgeStore) LoadForge(_ context.Context, id int64) (overload.ForgeConnection, error) {
	for _, connection := range store.connections {
		if connection.ID == id {
			return connection, nil
		}
	}
	return overload.ForgeConnection{}, postgres.ErrUnknownForge
}

func (store *forgeStore) LoadForgeByName(_ context.Context, name string) (overload.ForgeConnection, error) {
	for _, connection := range store.connections {
		if connection.Name == name {
			return connection, nil
		}
	}
	return overload.ForgeConnection{}, postgres.ErrUnknownForge
}

func (store *forgeStore) DeleteForge(context.Context, int64) error { return nil }

func (store *forgeStore) ListRepositories(context.Context) ([]overload.Repository, error) {
	return store.repos, nil
}

func (store *forgeStore) IngestForgePR(_ context.Context, connection overload.ForgeConnection, deliveryID string, _ []byte, event forge.Event) (bool, error) {
	if connection.ID == 0 || deliveryID == "" {
		return false, errors.New("bad ingest")
	}
	store.ingested = append(store.ingested, event)
	return true, nil
}

// stubForge accepts one token and records webhooks it is asked to create.
type stubForge struct {
	forge.Client
	token string
	hooks []string
}

func (client *stubForge) Identity(context.Context) (string, error) {
	if client.token != "good-token" {
		return "", errors.New("HTTP 401: 401 Unauthorized")
	}
	return "overload-bot", nil
}

func (client *stubForge) CreateWebhook(_ context.Context, repository, hookURL, secret string) error {
	client.hooks = append(client.hooks, repository+" "+hookURL+" "+secret)
	return nil
}

func TestForgeWebhook(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &forgeStore{connections: []overload.ForgeConnection{{ID: 3, Name: "gitlab", Kind: overload.ForgeGitLab, BaseURL: "https://gitlab.example.com", Token: "t", WebhookSecret: "s3cret"}}}
	handler := Handler(store)
	mergeRequest := `{"object_kind":"merge_request","project":{"id":42,"path_with_namespace":"group/api"},"object_attributes":{"iid":7,"action":"open","state":"opened","last_commit":{"id":"` + strings.Repeat("b", 40) + `"}}}`
	for _, test := range []struct {
		name, path, token, event, body string
		want                           int
	}{
		{"wrong secret", "/webhooks/gitlab/3", "nope", "Merge Request Hook", mergeRequest, http.StatusUnauthorized},
		{"wrong kind", "/webhooks/forgejo/3", "s3cret", "Merge Request Hook", mergeRequest, http.StatusUnauthorized},
		{"unknown connection", "/webhooks/gitlab/9", "s3cret", "Merge Request Hook", mergeRequest, http.StatusUnauthorized},
		{"push ignored", "/webhooks/gitlab/3", "s3cret", "Push Hook", `{"object_kind":"push"}`, http.StatusAccepted},
		{"merge request", "/webhooks/gitlab/3", "s3cret", "Merge Request Hook", mergeRequest, http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("X-Gitlab-Token", test.token)
			request.Header.Set("X-Gitlab-Event", test.event)
			request.Header.Set("X-Gitlab-Event-UUID", "uuid-"+test.name)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
	if len(store.ingested) != 1 || store.ingested[0].Action != "opened" || !store.ingested[0].Eligible || store.ingested[0].Repository != "group/api" {
		t.Fatalf("ingested = %+v", store.ingested)
	}
}

func TestForgeSetupPage(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	t.Setenv("OVERLOAD_BASE_URL", "https://overload.example")
	stub := &stubForge{}
	newForgeClient = func(connection overload.ForgeConnection) (forge.Client, error) {
		stub.token = connection.Token
		return stub, nil
	}
	t.Cleanup(func() { newForgeClient = forge.New })
	store := &forgeStore{}
	handler := Handler(store)
	get := func(path string) string {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Host = "overload.example"
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	post := func(path string, form url.Values, want int) string {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Host = "overload.example"
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s: %d want %d: %s", path, response.Code, want, response.Body.String())
		}
		body, _ := io.ReadAll(response.Body)
		return string(body)
	}
	page := get("/setup/forges")
	token := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page)
	if token == nil || !strings.Contains(page, "Add a git host") {
		t.Fatal("setup page has no form")
	}
	form := url.Values{"csrf": {token[1]}, "name": {"codeberg"}, "kind": {"forgejo"}, "base_url": {"https://codeberg.org"}, "token": {"bad-token"}}
	if body := post("/setup/forges", form, http.StatusUnprocessableEntity); !strings.Contains(body, "was not accepted") {
		t.Fatalf("bad token not reported: %s", body)
	}
	form.Set("token", "good-token")
	post("/setup/forges", form, http.StatusSeeOther)
	if len(store.connections) != 1 || store.connections[0].Token != "good-token" {
		t.Fatalf("connections = %+v", store.connections)
	}
	store.repos = []overload.Repository{{ID: 5, ForgeID: 1, Forge: "codeberg", ForgeKind: overload.ForgeForgejo, FullName: "acme/api", Enabled: true}}
	page = get("/setup/forges")
	if !strings.Contains(page, "https://overload.example/webhooks/forgejo/1") || !strings.Contains(page, "acme/api") {
		t.Fatalf("connection not listed:\n%s", page)
	}
	if strings.Contains(page, "good-token") || strings.Contains(page, "generated-secret") {
		t.Fatal("page shows a credential")
	}
	post("/configure/repositories/5/webhook", url.Values{"csrf": {token[1]}}, http.StatusSeeOther)
	if len(stub.hooks) != 1 || stub.hooks[0] != "acme/api https://overload.example/webhooks/forgejo/1 generated-secret" {
		t.Fatalf("hooks = %v", stub.hooks)
	}
}

func TestSavedRepositoryIsScopedToItsHost(t *testing.T) {
	repos := []overload.Repository{{ID: 1, FullName: "acme/api"}, {ID: 2, FullName: "acme/api", Forge: "codeberg", ForgeKind: overload.ForgeForgejo}}
	for _, test := range []struct {
		name     string
		resource string
		form     url.Values
		want     int64
	}{
		{"edit GitHub", "repositories", url.Values{"id": {"1"}, "name": {"acme/api"}}, 1},
		{"edit Forgejo", "repositories", url.Values{"id": {"2"}, "name": {"acme/api"}}, 2},
		{"new Forgejo", "repositories", url.Values{"name": {"acme/api"}, "forge": {"codeberg"}}, 2},
		{"new GitHub", "repositories", url.Values{"name": {"acme/api"}}, 1},
		{"binding on GitHub", "bindings", url.Values{"repository": {"acme/api"}, "repository_id": {"1"}}, 1},
		{"binding on Forgejo", "bindings", url.Values{"repository": {"acme/api"}, "repository_id": {"2"}}, 2},
	} {
		if got := savedRepository(test.resource, test.form, repos); got != test.want {
			t.Errorf("%s: %d, want %d", test.name, got, test.want)
		}
	}
	if repos[1].Ref() != "codeberg:acme/api" || repos[0].Ref() != "acme/api" {
		t.Fatalf("refs = %q %q", repos[0].Ref(), repos[1].Ref())
	}
}
