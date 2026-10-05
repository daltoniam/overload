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

type automationStore struct {
	crudStore
	prompts   []overload.PromptTemplate
	workflows []overload.Workflow
	schedules []overload.Schedule
}

func (store *automationStore) ListPrompts(context.Context) ([]overload.PromptTemplate, error) {
	return store.prompts, nil
}
func (store *automationStore) GetPrompt(_ context.Context, kind, name string, revision int) (overload.PromptTemplate, error) {
	for _, prompt := range store.prompts {
		if prompt.Kind == kind && prompt.Name == name && (revision == 0 || prompt.Revision == revision) {
			return prompt, nil
		}
	}
	return overload.PromptTemplate{}, http.ErrMissingFile
}
func (store *automationStore) SavePrompt(_ context.Context, prompt overload.PromptTemplate) (overload.PromptTemplate, error) {
	for index, current := range store.prompts {
		if current.Name == prompt.Name && current.Kind == prompt.Kind {
			prompt.Revision = current.Revision + 1
			store.prompts[index] = prompt
			return prompt, nil
		}
	}
	prompt.Revision = 1
	store.prompts = append(store.prompts, prompt)
	return prompt, nil
}
func (store *automationStore) DeletePrompt(_ context.Context, kind, name string) error {
	for index, prompt := range store.prompts {
		if prompt.Kind == kind && prompt.Name == name {
			store.prompts = append(store.prompts[:index], store.prompts[index+1:]...)
			return nil
		}
	}
	return http.ErrMissingFile
}
func (store *automationStore) ListWorkflows(context.Context) ([]overload.Workflow, error) {
	return store.workflows, nil
}
func (store *automationStore) SaveWorkflow(_ context.Context, workflow overload.Workflow) error {
	for index, current := range store.workflows {
		if current.Name == workflow.Name {
			store.workflows[index] = workflow
			return nil
		}
	}
	store.workflows = append(store.workflows, workflow)
	return nil
}
func (store *automationStore) DeleteWorkflow(_ context.Context, name string) error {
	for index, workflow := range store.workflows {
		if workflow.Name == name {
			store.workflows = append(store.workflows[:index], store.workflows[index+1:]...)
			return nil
		}
	}
	return http.ErrMissingFile
}
func (store *automationStore) ListSchedules(context.Context) ([]overload.Schedule, error) {
	return store.schedules, nil
}
func (store *automationStore) SaveSchedule(_ context.Context, schedule overload.Schedule) error {
	for index, current := range store.schedules {
		if current.Name == schedule.Name {
			store.schedules[index] = schedule
			return nil
		}
	}
	store.schedules = append(store.schedules, schedule)
	return nil
}
func (store *automationStore) DeleteSchedule(_ context.Context, name string) error {
	for index, schedule := range store.schedules {
		if schedule.Name == name {
			store.schedules = append(store.schedules[:index], store.schedules[index+1:]...)
			return nil
		}
	}
	return http.ErrMissingFile
}

func TestAutomationCRUDPages(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &automationStore{prompts: []overload.PromptTemplate{{Name: "base", Kind: "entry", Body: "Original", Revision: 1}}, workflows: []overload.Workflow{{Name: "review", Kind: "pr_review", Agents: []string{"agent"}, Enabled: true}}, schedules: []overload.Schedule{{Name: "daily", Workflow: "scheduled", Cron: "0 9 * * *", Timezone: "UTC", Input: []byte(`{"topic":"status"}`), Enabled: true}}}
	handler := Handler(store)
	for _, test := range []struct {
		path, text string
		status     int
	}{
		{"/configure/prompts", "View / edit", http.StatusOK},
		{"/configure/prompts/new", "New prompt", http.StatusOK},
		{"/configure/prompts/entry/base", "Original", http.StatusOK},
		{"/configure/prompts/entry/missing", "404", http.StatusNotFound},
		{"/configure/workflows", "review", http.StatusOK},
		{"/configure/workflows/new", "New workflow", http.StatusOK},
		{"/configure/workflows/review", "Save workflow", http.StatusOK},
		{"/configure/workflows/missing", "404", http.StatusNotFound},
		{"/configure/schedules", "daily", http.StatusOK},
		{"/configure/schedules/new", "New schedule", http.StatusOK},
		{"/configure/schedules/daily", "status", http.StatusOK},
		{"/configure/schedules/missing", "404", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.text) {
			t.Errorf("%s: %d missing %q", test.path, response.Code, test.text)
		}
	}
	formPage := httptest.NewRecorder()
	handler.ServeHTTP(formPage, httptest.NewRequest(http.MethodGet, "/configure/prompts/entry/base", nil))
	if strings.Contains(formPage.Body.String(), `<select name="kind"`) || !strings.Contains(formPage.Body.String(), `value="Review focus"`) && !strings.Contains(formPage.Body.String(), `value="Entry"`) || !strings.Contains(formPage.Body.String(), `class="danger"`) {
		t.Fatal("existing prompt type or delete button rendered incorrectly")
	}
	match := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(formPage.Body.String())
	if len(match) != 2 {
		t.Fatal("missing CSRF token")
	}
	post := func(path string, form url.Values, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Errorf("%s: %d want %d: %s", path, response.Code, want, response.Body.String())
		}
	}
	csrf := match[1]
	post("/configure/prompts", url.Values{"name": {"base"}, "kind": {"entry"}, "body": {"Changed"}}, http.StatusForbidden)
	post("/configure/prompts", url.Values{"csrf": {csrf}, "existing": {"entry/base"}, "name": {"other"}, "kind": {"entry"}, "body": {"Changed"}}, http.StatusBadRequest)
	post("/configure/prompts", url.Values{"csrf": {csrf}, "existing": {"entry/base"}, "name": {"base"}, "kind": {"entry"}, "body": {"Changed"}}, http.StatusSeeOther)
	if store.prompts[0].Revision != 2 || store.prompts[0].Body != "Changed" {
		t.Fatalf("prompt: %+v", store.prompts)
	}
	post("/configure/workflows", url.Values{"csrf": {csrf}, "existing": {"review"}, "name": {"other"}, "kind": {"pr_review"}, "agents": {"agent"}}, http.StatusBadRequest)
	post("/configure/workflows", url.Values{"csrf": {csrf}, "existing": {"review"}, "name": {"review"}, "kind": {"pr_review"}, "agents": {"agent"}}, http.StatusSeeOther)
	if store.workflows[0].Enabled {
		t.Fatal("workflow not disabled")
	}
	post("/configure/schedules", url.Values{"csrf": {csrf}, "existing": {"daily"}, "name": {"other"}, "workflow": {"scheduled"}}, http.StatusBadRequest)
	post("/configure/schedules", url.Values{"csrf": {csrf}, "existing": {"daily"}, "name": {"daily"}, "workflow": {"scheduled"}, "cron": {"0 8 * * *"}, "timezone": {"UTC"}, "input": {`{"topic":"status"}`}}, http.StatusSeeOther)
	if store.schedules[0].Enabled || store.schedules[0].Cron != "0 8 * * *" {
		t.Fatalf("schedule: %+v", store.schedules)
	}
	post("/configure/prompts/entry/base/delete", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusSeeOther)
	post("/configure/workflows/review/delete", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusSeeOther)
	post("/configure/schedules/daily/delete", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusSeeOther)
	if len(store.prompts)+len(store.workflows)+len(store.schedules) != 0 {
		t.Fatal("delete did not remove resources")
	}
}
