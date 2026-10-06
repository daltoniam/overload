package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

type crudStore struct {
	fakeConfigStore
	agents   []overload.AgentDefinition
	repos    []overload.Repository
	bindings []overload.TriggerBinding
}

func (store *crudStore) ListAgents(context.Context) ([]overload.AgentDefinition, error) {
	return store.agents, nil
}
func (store *crudStore) SaveAgent(_ context.Context, agent overload.AgentDefinition) error {
	for index := range store.agents {
		if store.agents[index].Name == agent.Name {
			store.agents[index] = agent
			return nil
		}
	}
	store.agents = append(store.agents, agent)
	return nil
}
func (store *crudStore) DeleteAgent(_ context.Context, name string) error {
	for index, agent := range store.agents {
		if agent.Name == name {
			store.agents = append(store.agents[:index], store.agents[index+1:]...)
			break
		}
	}
	return nil
}
func (store *crudStore) ListRepositories(context.Context) ([]overload.Repository, error) {
	return store.repos, nil
}
func (store *crudStore) SaveRepository(_ context.Context, name string, enabled, dryRun bool) error {
	for index := range store.repos {
		if store.repos[index].FullName == name {
			store.repos[index].Enabled = enabled
			store.repos[index].DryRun = dryRun
			return nil
		}
	}
	store.repos = append(store.repos, overload.Repository{ID: int64(len(store.repos) + 1), FullName: name, Enabled: enabled, DryRun: dryRun})
	return nil
}
func (store *crudStore) DeleteRepository(_ context.Context, id int64) error {
	for index, repo := range store.repos {
		if repo.ID == id {
			store.repos = append(store.repos[:index], store.repos[index+1:]...)
			break
		}
	}
	return nil
}
func (store *crudStore) ListBindings(context.Context) ([]overload.TriggerBinding, error) {
	return store.bindings, nil
}
func (store *crudStore) SaveBinding(_ context.Context, binding overload.TriggerBinding) error {
	store.bindings = append(store.bindings, binding)
	return nil
}

func TestConfigurationCRUDPages(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &crudStore{agents: []overload.AgentDefinition{{Name: "reviewer", Model: "local", Prompt: "Review carefully.", PromptRevision: 2, Enabled: true}}, repos: []overload.Repository{{ID: 1, FullName: "owner/repo", Enabled: true, DryRun: true}}}
	handler := Handler(store)
	for _, test := range []struct {
		path, text string
		status     int
	}{
		{"/", "Run statistics", http.StatusOK},
		{"/configure", "See Other", http.StatusSeeOther},
		{"/configure/agents", "View / edit", http.StatusOK},
		{"/configure/agents/new", "New agent", http.StatusOK},
		{"/configure/agents/reviewer", "Edit agent", http.StatusOK},
		{"/configure/agents/missing", "404", http.StatusNotFound},
		{"/configure/repositories", "owner/repo", http.StatusOK},
		{"/configure/repositories/new", "New repository", http.StatusOK},
		{"/configure/repositories/1", "PR event bindings", http.StatusOK},
		{"/configure/repositories/999", "404", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.text) {
			t.Errorf("%s: %d %s", test.path, response.Code, response.Body.String())
		}
	}
	for query, visible := range map[string]bool{"kind=pr_review": true, "kind=scheduled_prompt": false, "status=Enabled": true, "status=Disabled": false} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/configure/agents?"+query, nil))
		if strings.Contains(response.Body.String(), "<strong>reviewer</strong>") != visible {
			t.Errorf("agents?%s: reviewer visible=%v", query, !visible)
		}
	}
	formPage := httptest.NewRecorder()
	handler.ServeHTTP(formPage, httptest.NewRequest(http.MethodGet, "/configure/agents/reviewer", nil))
	token := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(formPage.Body.String())
	if len(token) != 2 {
		t.Fatal("missing csrf token")
	}
	post := func(path string, form url.Values, want int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Errorf("%s: %d want %d: %s", path, response.Code, want, response.Body.String())
		}
		return response
	}
	if page := formPage.Body.String(); !strings.Contains(page, "Review carefully.") || !strings.Contains(page, "Version 2.") || strings.Contains(page, "Start from") {
		t.Fatal("agent form does not show its instructions and version")
	}
	if page := httptest.NewRecorder(); true {
		handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/configure/agents/new?start=context", nil))
		if body := page.Body.String(); !strings.Contains(body, "Start from") || !strings.Contains(body, "You are reviewing a pull request for real, actionable bugs") {
			t.Fatal("new agent form does not start from a built-in prompt")
		}
	}
	post("/configure/agents", url.Values{"name": {"reviewer"}, "model": {"local"}, "prompt": {"x"}}, http.StatusForbidden)
	post("/configure/agents", url.Values{"csrf": {token[1]}, "name": {"reviewer"}, "model": {"local"}, "prompt": {"Overwrite."}}, http.StatusBadRequest)
	if response := post("/configure/agents", url.Values{"csrf": {token[1]}, "existing": {"reviewer"}, "name": {"reviewer"}, "model": {"local"}, "prompt": {"  "}}, http.StatusBadRequest); !strings.Contains(response.Body.String(), "the agent prompt is required") {
		t.Fatalf("empty prompt message: %s", response.Body.String())
	}
	post("/configure/agents", url.Values{"csrf": {token[1]}, "existing": {"reviewer"}, "name": {"reviewer"}, "model": {"other"}, "prompt": {"Line one.\r\nLine two."}}, http.StatusSeeOther)
	if store.agents[0].Model != "other" || store.agents[0].Enabled || store.agents[0].Prompt != "Line one.\nLine two." {
		t.Fatalf("agent edit: %+v", store.agents[0])
	}
	post("/configure/repositories", url.Values{"csrf": {token[1]}, "id": {"1"}, "name": {"other/repo"}}, http.StatusBadRequest)
	post("/configure/repositories", url.Values{"csrf": {token[1]}, "id": {"1"}, "name": {"owner/repo"}}, http.StatusSeeOther)
	if store.repos[0].Enabled || !store.repos[0].DryRun {
		t.Fatal("repository not disabled or posting enabled without opt-in")
	}
	post("/configure/repositories", url.Values{"csrf": {token[1]}, "id": {"1"}, "name": {"owner/repo"}, "post": {"true"}}, http.StatusSeeOther)
	if store.repos[0].DryRun {
		t.Fatal("posting opt-in not saved")
	}
	post("/configure/agents/reviewer/delete", url.Values{"csrf": {token[1]}, "confirmed": {"true"}}, http.StatusSeeOther)
	post("/configure/repositories/1/delete", url.Values{"csrf": {token[1]}, "confirmed": {"true"}}, http.StatusSeeOther)
	if len(store.agents) != 0 || len(store.repos) != 0 {
		t.Fatalf("deletion failed: agents=%v repos=%v", store.agents, store.repos)
	}
}
