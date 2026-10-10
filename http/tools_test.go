package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/queue"
)

type toolStore struct {
	automationStore
	servers []overload.ToolServer
	ran     []string
	usedBy  map[string]string
}

func (store *toolStore) ListToolServers(context.Context) ([]overload.ToolServer, error) {
	return store.servers, nil
}
func (store *toolStore) GetToolServer(_ context.Context, name string) (overload.ToolServer, error) {
	for _, server := range store.servers {
		if server.Name == name {
			return server, nil
		}
	}
	return overload.ToolServer{}, errors.New("not found")
}
func (store *toolStore) SaveToolServer(_ context.Context, server overload.ToolServer) error {
	for index := range store.servers {
		if store.servers[index].Name == server.Name {
			store.servers[index] = server
			return nil
		}
	}
	store.servers = append(store.servers, server)
	return nil
}
func (store *toolStore) DeleteToolServer(_ context.Context, name string) error {
	if agent := store.usedBy[name]; agent != "" {
		return fmt.Errorf("%w: agent %s uses it", postgres.ErrToolServerInUse, agent)
	}
	for index, server := range store.servers {
		if server.Name == name {
			store.servers = append(store.servers[:index], store.servers[index+1:]...)
			return nil
		}
	}
	return errors.New("not found")
}
func (store *toolStore) RunScheduleNow(_ context.Context, name string) (int64, error) {
	store.ran = append(store.ran, name)
	return 77, nil
}

func TestToolServerPages(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &toolStore{usedBy: map[string]string{"switchboard": "researcher"}, servers: []overload.ToolServer{{Name: "switchboard", URL: "https://app.switchboard-mcp.com/orgs/acme/mcp", TokenEnv: "OVERLOAD_TOOL_SWITCHBOARD", Enabled: true}}}
	store.schedules = []overload.Schedule{{Name: "research", Workflow: "research", Cron: "0 7 * * *", Timezone: "America/Chicago", Input: []byte(`{}`), Enabled: true}}
	original := listTools
	t.Cleanup(func() { listTools = original })
	listTools = func(_ context.Context, server overload.ToolServer) ([]string, error) {
		if server.Name == "switchboard" {
			return []string{"search", "execute"}, nil
		}
		return nil, errors.New("token variable OVERLOAD_TOOL_LOCAL is not set")
	}
	handler := Handler(store)
	for _, test := range []struct {
		path, text string
		status     int
	}{
		{"/configure/tools", "app.switchboard-mcp.com", http.StatusOK},
		{"/configure/tools/new", "New tool server", http.StatusOK},
		{"/configure/tools/switchboard", "OVERLOAD_TOOL_SWITCHBOARD", http.StatusOK},
		{"/configure/tools/missing", "404", http.StatusNotFound},
		{"/configure/schedules/research", "Run now", http.StatusOK},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.text) {
			t.Errorf("%s: %d missing %q", test.path, response.Code, test.text)
		}
	}
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/configure/tools/switchboard", nil))
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())[1]
	post := func(path string, form url.Values, want int, contains string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want || !strings.Contains(response.Body.String()+response.Header().Get("Location"), contains) {
			t.Errorf("%s: %d want %d, missing %q: %s", path, response.Code, want, contains, response.Body.String())
		}
	}
	post("/configure/tools", url.Values{"name": {"local"}, "url": {"http://127.0.0.1:3847/mcp"}}, http.StatusForbidden, "")
	post("/configure/tools", url.Values{"csrf": {csrf}, "name": {"local"}, "url": {"http://127.0.0.1:3847/mcp"}, "token_env": {"GITHUB_TOKEN"}}, http.StatusBadRequest, "OVERLOAD_TOOL_")
	post("/configure/tools", url.Values{"csrf": {csrf}, "name": {"switchboard"}, "url": {"http://127.0.0.1:3847/mcp"}}, http.StatusBadRequest, "already exists")
	post("/configure/tools", url.Values{"csrf": {csrf}, "existing": {"switchboard"}, "name": {"other"}, "url": {"http://127.0.0.1:3847/mcp"}}, http.StatusBadRequest, "cannot be changed")
	post("/configure/tools", url.Values{"csrf": {csrf}, "name": {"local"}, "url": {"http://127.0.0.1:3847/mcp"}, "token_env": {"OVERLOAD_TOOL_LOCAL"}, "enabled": {"true"}}, http.StatusSeeOther, "/configure/tools/local")
	if len(store.servers) != 2 || !store.servers[1].Enabled || store.servers[1].TokenEnv != "OVERLOAD_TOOL_LOCAL" {
		t.Fatalf("servers %+v", store.servers)
	}
	for path, want := range map[string]string{"/configure/tools/switchboard?check=1": "<code>execute</code>", "/configure/tools/local?check=1": "OVERLOAD_TOOL_LOCAL is not set"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Errorf("%s: %d missing %q", path, response.Code, want)
		}
	}
	post("/configure/tools/switchboard/delete", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusConflict, "researcher")
	post("/configure/tools/local/delete", url.Values{"csrf": {csrf}, "confirmed": {"true"}}, http.StatusSeeOther, "/configure/tools")
	post("/configure/schedules/research/run", url.Values{"csrf": {csrf}}, http.StatusSeeOther, "/runs/77")
	if len(store.ran) != 1 || store.ran[0] != "research" {
		t.Fatalf("ran %v", store.ran)
	}
}

type reviewRequestStore struct {
	crudStore
	requested []string
	fail      error
}

func (store *reviewRequestStore) RequestReview(_ context.Context, repositoryID int64, number int, again bool) (int64, error) {
	store.requested = append(store.requested, fmt.Sprintf("%d#%d again=%v", repositoryID, number, again))
	return 91, store.fail
}

func TestReviewNowForm(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &reviewRequestStore{crudStore: crudStore{repos: []overload.Repository{{ID: 1, FullName: "owner/repo", Enabled: true}}}}
	handler := Handler(store)
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/configure/repositories/%d", store.repos[0].ID), nil))
	if !strings.Contains(page.Body.String(), "Review now") {
		t.Fatalf("repository page has no Review now form: %d", page.Code)
	}
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())[1]
	post := func(form url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/configure/repositories/%d/review", store.repos[0].ID), strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	repo := store.repos[0].FullName
	if response := post(url.Values{"repository": {repo}, "pr": {"296"}}); response.Code != http.StatusForbidden {
		t.Fatalf("no CSRF token: %d", response.Code)
	}
	if response := post(url.Values{"csrf": {csrf}, "repository": {repo}, "pr": {"abc"}}); response.Code != http.StatusBadRequest {
		t.Fatalf("bad number: %d", response.Code)
	}
	if response := post(url.Values{"csrf": {csrf}, "pr": {"#296"}, "again": {"true"}}); response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/runs/91") {
		t.Fatalf("queue: %d %s", response.Code, response.Header().Get("Location"))
	}
	store.fail = fmt.Errorf("%w: #296 is a draft", queue.ErrReviewNotQueued)
	if response := post(url.Values{"csrf": {csrf}, "repository": {repo}, "pr": {"296"}}); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "is a draft") {
		t.Fatalf("refusal: %d %s", response.Code, response.Body.String())
	}
	if strings.Join(store.requested, ",") != "1#296 again=true,1#296 again=false" {
		t.Fatalf("requested %v", store.requested)
	}
}
