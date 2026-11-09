package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

type fakeNodeStore struct {
	fakeStore
	nodes []overload.Node
}

func (store *fakeNodeStore) AuthenticateAPIKey(_ context.Context, secret string) (overload.APIKey, *overload.User, error) {
	switch secret {
	case "ovl_node":
		return overload.APIKey{ID: 1, Scopes: []string{overload.ScopeNodes}}, nil, nil
	case "ovl_models":
		return overload.APIKey{ID: 2, Scopes: []string{overload.ScopeModels}}, nil, nil
	}
	return overload.APIKey{}, nil, pgx.ErrNoRows
}
func (store *fakeNodeStore) ReportNode(_ context.Context, report overload.NodeReport) error {
	if err := report.Validate(); err != nil {
		return err
	}
	store.nodes = append(store.nodes, overload.Node{Report: report, LastSeen: time.Now()})
	return nil
}
func (store *fakeNodeStore) ListNodes(context.Context) ([]overload.Node, error) {
	return store.nodes, nil
}
func (store *fakeNodeStore) DeleteNode(context.Context, string) error { return nil }
func (store *fakeNodeStore) ListPlacements(context.Context) ([]overload.Placement, error) {
	return []overload.Placement{{Name: "beam", Model: "mlx-community/Beam-4bit", Nodes: []string{"a", "b"}, Rank0IP: "10.0.0.1", RDMA: [][]string{{"", "rdma_en5"}, {"rdma_en5", ""}}}}, nil
}

func TestNodesAPIAndPage(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &fakeNodeStore{}
	handler := Handler(store)
	report := func(key, body string) int {
		request := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(body))
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	for name, test := range map[string]struct {
		key, body string
		status    int
	}{
		"no key":        {"", `{"name":"a"}`, http.StatusUnauthorized},
		"wrong scope":   {"ovl_models", `{"name":"a"}`, http.StatusUnauthorized},
		"unknown field": {"ovl_node", `{"name":"a","root":true}`, http.StatusBadRequest},
		"invalid":       {"ovl_node", `{"name":"a b"}`, http.StatusUnprocessableEntity},
		"first":         {"ovl_node", `{"name":"a","os":"darwin","os_version":"26.2","llama_build":12058}`, http.StatusNoContent},
		"second":        {"ovl_node", `{"name":"b","os":"darwin","os_version":"26.2","llama_build":12058}`, http.StatusNoContent},
	} {
		if status := report(test.key, test.body); status != test.status {
			t.Errorf("%s: %d", name, status)
		}
	}
	report("ovl_node", `{"name":"c","os":"darwin","os_version":"26.1","llama_build":12058}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "operating system version differs on c") || !strings.Contains(body, "mlx.launch --backend jaccl") || !strings.Contains(body, "llama.cpp b12058") {
		t.Fatalf("nodes page: %d %s", response.Code, body)
	}
}
