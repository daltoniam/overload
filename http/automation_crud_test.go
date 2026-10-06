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
	workflows []overload.Workflow
	schedules []overload.Schedule
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
	store := &automationStore{workflows: []overload.Workflow{{Name: "review", Kind: "pr_review", Agents: []string{"agent"}, Enabled: true}}, schedules: []overload.Schedule{{Name: "daily", Workflow: "scheduled", Cron: "0 9 * * *", Timezone: "UTC", Input: []byte(`{"topic":"status"}`), Enabled: true}}}
	handler := Handler(store)
	for _, test := range []struct {
		path, text string
		status     int
	}{
		{"/configure/prompts", "See Other", http.StatusSeeOther},
		{"/configure/prompts/entry/base", "See Other", http.StatusSeeOther},
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
	handler.ServeHTTP(formPage, httptest.NewRequest(http.MethodGet, "/configure/workflows/review", nil))
	if !strings.Contains(formPage.Body.String(), `class="danger"`) || strings.Contains(formPage.Body.String(), `href="/configure/prompts"`) {
		t.Fatal("workflow delete button missing or prompts still linked")
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
	post("/configure/workflows", url.Values{"name": {"review"}, "kind": {"pr_review"}, "main_agent": {"agent"}}, http.StatusForbidden)
	post("/configure/workflows", url.Values{"csrf": {csrf}, "name": {"review"}, "kind": {"pr_review"}, "main_agent": {"agent"}}, http.StatusBadRequest)
	post("/configure/workflows", url.Values{"csrf": {csrf}, "existing": {"review"}, "name": {"other"}, "kind": {"pr_review"}, "main_agent": {"agent"}}, http.StatusBadRequest)
	post("/configure/workflows", url.Values{"csrf": {csrf}, "existing": {"review"}, "name": {"review"}, "kind": {"pr_review"}, "main_agent": {"agent"}}, http.StatusSeeOther)
	if store.workflows[0].Enabled {
		t.Fatal("workflow not disabled")
	}
	post("/configure/schedules", url.Values{"csrf": {csrf}, "existing": {"daily"}, "name": {"other"}, "workflow": {"scheduled"}}, http.StatusBadRequest)
	post("/configure/schedules", url.Values{"csrf": {csrf}, "existing": {"daily"}, "name": {"daily"}, "workflow": {"scheduled"}, "cron": {"0 8 * * *"}, "timezone": {"UTC"}, "input": {`{"topic":"status"}`}}, http.StatusSeeOther)
	if store.schedules[0].Enabled || store.schedules[0].Cron != "0 8 * * *" {
		t.Fatalf("schedule: %+v", store.schedules)
	}
	post("/configure/workflows/review/delete", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusSeeOther)
	post("/configure/schedules/daily/delete", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusSeeOther)
	if len(store.workflows)+len(store.schedules) != 0 {
		t.Fatal("delete did not remove resources")
	}
}
