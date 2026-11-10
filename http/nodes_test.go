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
	"time"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// fakeNodeStore claims names for the first key that reports them, like the
// Postgres store.
type fakeNodeStore struct {
	fakeStore
	nodes      []overload.Node
	owners     map[string]int64
	reportErr  error
	placements []overload.Placement
}

func (store *fakeNodeStore) AuthenticateAPIKey(_ context.Context, secret string) (overload.APIKey, *overload.User, error) {
	switch secret {
	case "ovl_node":
		return overload.APIKey{ID: 1, Scopes: []string{overload.ScopeNodes}}, nil, nil
	case "ovl_other":
		return overload.APIKey{ID: 3, Scopes: []string{overload.ScopeNodes}}, &overload.User{ID: 1, Role: overload.RoleAdmin}, nil
	case "ovl_member":
		return overload.APIKey{ID: 4, Scopes: []string{overload.ScopeNodes}}, &overload.User{ID: 2, Role: overload.RoleMember}, nil
	case "ovl_models":
		return overload.APIKey{ID: 2, Scopes: []string{overload.ScopeModels}}, nil, nil
	}
	return overload.APIKey{}, nil, overload.ErrInvalidAPIKey
}
func (store *fakeNodeStore) ReportNode(_ context.Context, keyID int64, report overload.NodeReport) error {
	if store.reportErr != nil {
		return store.reportErr
	}
	if err := report.Validate(); err != nil {
		return err
	}
	if store.owners == nil {
		store.owners = map[string]int64{}
	}
	if owner, ok := store.owners[report.Name]; ok && owner != keyID {
		return overload.ErrNodeClaimed
	}
	store.owners[report.Name] = keyID
	store.nodes = append(store.nodes, overload.Node{Report: report, LastSeen: time.Now()})
	return nil
}
func (store *fakeNodeStore) ListNodes(context.Context) ([]overload.Node, error) {
	return store.nodes, nil
}
func (store *fakeNodeStore) DeleteNode(_ context.Context, name string) error {
	for _, placement := range store.placements {
		for _, node := range placement.Nodes {
			if node == name {
				return fmt.Errorf("%w: placement %s uses %s; delete the placement first", overload.ErrNodeInPlacement, placement.Name, name)
			}
		}
	}
	for index, node := range store.nodes {
		if node.Report.Name == name {
			store.nodes = append(store.nodes[:index], store.nodes[index+1:]...)
			delete(store.owners, name)
			return nil
		}
	}
	return pgx.ErrNoRows
}
func (store *fakeNodeStore) ReleaseNode(_ context.Context, name string) error {
	for index, node := range store.nodes {
		if node.Report.Name == name {
			store.nodes[index].KeyPrefix = ""
			delete(store.owners, name)
			return nil
		}
	}
	return pgx.ErrNoRows
}
func (store *fakeNodeStore) ListPlacements(context.Context) ([]overload.Placement, error) {
	return store.placements, nil
}

func TestNodesAPI(t *testing.T) {
	store := &fakeNodeStore{}
	handler := Handler(store)
	report := func(key, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(body))
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for _, test := range []struct {
		name, key, body string
		status          int
	}{
		{"no key", "", `{"name":"a"}`, http.StatusUnauthorized},
		{"wrong scope", "ovl_models", `{"name":"a"}`, http.StatusUnauthorized},
		{"member's key", "ovl_member", `{"name":"a"}`, http.StatusForbidden},
		{"newer node's extra field", "ovl_node", `{"name":"a","os":"darwin","os_version":"26.2","llama_build":12058,"gpu_cores":80}`, http.StatusNoContent},
		{"trailing data", "ovl_node", `{"name":"a"}{"name":"b"}`, http.StatusBadRequest},
		{"stray closing bracket", "ovl_node", `{"name":"a"}}`, http.StatusBadRequest},
		{"trailing whitespace", "ovl_node", "{\"name\":\"a\"}\n", http.StatusNoContent},
		{"too large", "ovl_node", `{"name":"a","chip":"` + strings.Repeat("x", maxNodeReport) + `"}`, http.StatusBadRequest},
		{"invalid", "ovl_node", `{"name":"a b"}`, http.StatusUnprocessableEntity},
		{"same key again", "ovl_node", `{"name":"a","os":"darwin","os_version":"26.2"}`, http.StatusNoContent},
		{"another key's node", "ovl_other", `{"name":"a","os":"darwin","os_version":"26.2"}`, http.StatusForbidden},
		{"admin's key, new node", "ovl_other", `{"name":"b","os":"darwin","os_version":"26.2"}`, http.StatusNoContent},
	} {
		if response := report(test.key, test.body); response.Code != test.status {
			t.Errorf("%s: %d %s", test.name, response.Code, response.Body.String())
		}
	}
	for _, test := range []struct {
		err     error
		status  int
		message string
	}{
		{fmt.Errorf("%w: the inventory already holds 500 nodes", overload.ErrTooManyNodes), http.StatusUnprocessableEntity, "500 nodes"},
		{overload.ErrNodeReleased, http.StatusForbidden, "released this node"},
		{errors.New("dial tcp 10.0.0.9:5432: connection refused"), http.StatusInternalServerError, "report not stored"},
	} {
		store.reportErr = test.err
		response := report("ovl_node", `{"name":"a"}`)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.message) || strings.Contains(response.Body.String(), "5432") {
			t.Errorf("%v: %d %s", test.err, response.Code, response.Body.String())
		}
	}
}

func TestNodesPage(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	now := time.Now()
	store := &fakeNodeStore{
		nodes: []overload.Node{
			{Report: overload.NodeReport{Name: "studio-1", OS: "darwin", OSVersion: "26.2", LlamaBuild: 12058, MemoryBytes: 64 << 30, Downloads: []string{"qwen3 Q4_K_M"}}, LastSeen: now, KeyPrefix: "ovl_ab12"},
			{Report: overload.NodeReport{Name: "studio-2", OS: "darwin", OSVersion: "26.2", LlamaBuild: 12058}, LastSeen: now},
			{Report: overload.NodeReport{Name: "c", OS: "darwin", OSVersion: "26.1", LlamaBuild: 12058}, LastSeen: now},
		},
		placements: []overload.Placement{{Name: "beam", Model: "mlx-community/Beam-4bit", Nodes: []string{"studio-1", "studio-2"}, Rank0IP: "10.0.0.1", RDMA: [][]string{{"", "rdma_en5"}, {"rdma_en5", ""}}}},
	}
	handler := Handler(store)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	body := response.Body.String()
	for _, want := range []string{"operating system version (darwin) differs on c", "mlx.launch --backend jaccl", "llama.cpp b12058", "64 GB memory", "1 downloaded model", "qwen3 Q4_K_M", "http://10.0.0.1:8080/v1", `aria-label="Forget studio-1"`, `data-confirm-title="Forget node studio-1"`, "Key ovl_ab12", `action="/nodes/studio-1/release"`, `data-confirm-title="Release node studio-1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("nodes page lacks %q: %d %s", want, response.Code, body)
		}
	}
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(body)[1]
	forget := func(name string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", csrf)
		request := httptest.NewRequest(http.MethodPost, "/nodes/"+name+"/delete", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := forget("c", url.Values{}); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Forget node c?") || !strings.Contains(response.Body.String(), "reappears if it reports again") || len(store.nodes) != 3 {
		t.Fatalf("forget without confirmation: %d %s", response.Code, response.Body.String())
	}
	if response := forget("c", url.Values{"confirmed": {"true"}}); response.Code != http.StatusSeeOther || len(store.nodes) != 2 {
		t.Fatalf("forget: %d %s", response.Code, response.Body.String())
	}
	if response := forget("c", url.Values{"confirmed": {"true"}}); response.Code != http.StatusNotFound {
		t.Fatalf("forget a missing node: %d", response.Code)
	}
	release := func(name string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", csrf)
		request := httptest.NewRequest(http.MethodPost, "/nodes/"+name+"/release", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := release("studio-1", url.Values{}); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Release node studio-1?") || store.nodes[0].KeyPrefix == "" {
		t.Fatalf("release without confirmation: %d %s", response.Code, response.Body.String())
	}
	if response := release("studio-1", url.Values{"confirmed": {"true"}}); response.Code != http.StatusSeeOther || store.nodes[0].KeyPrefix != "" {
		t.Fatalf("release: %d %s", response.Code, response.Body.String())
	}
	if response := release("missing", url.Values{"confirmed": {"true"}}); response.Code != http.StatusNotFound {
		t.Fatalf("release a missing node: %d", response.Code)
	}
	if response := forget("studio-1", url.Values{"confirmed": {"true"}}); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "placement beam uses studio-1") || len(store.nodes) != 2 {
		t.Fatalf("forget a placed node: %d %s", response.Code, response.Body.String())
	}
}

func TestOnlyAdminsCreateNodeKeys(t *testing.T) {
	store := &fakeKeyStore{}
	mux := http.NewServeMux()
	registerKeys(mux, store, "install-token")
	for _, test := range []struct {
		role   string
		status int
	}{
		{overload.RoleMember, http.StatusForbidden},
		{overload.RoleAdmin, http.StatusOK},
	} {
		form := url.Values{"csrf": {"person-token"}, "name": {"studio"}, "scopes": {"nodes"}, "days": {"30"}}
		request := httptest.NewRequest(http.MethodPost, "/keys", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request = request.WithContext(withIdentity(request.Context(), identity{user: &overload.User{ID: 7, Role: test.role}, role: test.role, formToken: "person-token"}))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Errorf("%s: %d %s", test.role, response.Code, response.Body.String())
		}
	}
}
