package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload/postgres"
)

type dashboardStore struct {
	fakeStore
	stats postgres.DashboardStats
	err   error
}

func (store *dashboardStore) DashboardStatistics(context.Context, time.Time) (postgres.DashboardStats, error) {
	return store.stats, store.err
}

func TestDashboardGraphs(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &dashboardStore{stats: postgres.DashboardStats{Available: true, Total: 123, Completed: 100, Failed: 20, Active: 3, Days: []postgres.ChartValue{{Label: "Oct 04", Count: 123}}, Outcomes: []postgres.ChartValue{{Label: "completed", Count: 100}}, Kinds: []postgres.ChartValue{{Label: "pr_review", Count: 123}}}}
	response := httptest.NewRecorder()
	Handler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, expected := range []string{"Run activity", "Run status", "Job types", "123", "View daily values", "aria-hidden=", "UTC"} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	if strings.Contains(response.Body.String(), "Manage models →") {
		t.Fatal("dashboard still consists of directory cards")
	}
	store.err = errors.New("unavailable")
	response = httptest.NewRecorder()
	Handler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != 500 {
		t.Fatal("aggregate failure must not render invented zero statistics")
	}
}
