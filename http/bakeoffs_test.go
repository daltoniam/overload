package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/postgres"
	"github.com/jackc/pgx/v5"
)

type fakeBakeoffStore struct {
	fakeStore
	queued [][]string
}

func (store *fakeBakeoffStore) ListSuites(context.Context) ([]eval.Suite, error) {
	return []eval.Suite{eval.PlantedSuite()}, nil
}
func (store *fakeBakeoffStore) ListBakeoffs(context.Context) ([]postgres.Bakeoff, error) {
	return []postgres.Bakeoff{{ID: 1, Status: "completed", Models: []overload.ModelProfile{{Name: "flint"}}, Suites: []eval.Suite{{Name: "planted"}}, Runs: 2, CreatedAt: time.Now()}}, nil
}
func (store *fakeBakeoffStore) GetBakeoff(_ context.Context, id int64) (postgres.Bakeoff, error) {
	if id != 1 {
		return postgres.Bakeoff{}, pgx.ErrNoRows
	}
	bakeoff, _ := store.ListBakeoffs(context.Background())
	result := bakeoff[0]
	result.Models = append(result.Models, overload.ModelProfile{Name: "gemma"})
	result.Results = []postgres.BakeoffResult{
		{Model: "flint", Suite: "planted", Case: "planted-invoice", Run: 1, Score: eval.Score{Expected: 5, Matched: 4, Misplaced: 1, Reported: 5, Unmatched: 1}, Duration: 6 * time.Minute},
		{Model: "gemma", Suite: "planted", Case: "planted-invoice", Run: 1, Score: eval.Score{Expected: 5}, Error: "model timed out"},
	}
	return result, nil
}
func (store *fakeBakeoffStore) QueueBakeoff(_ context.Context, models, suites []string, runs int) (int64, error) {
	if len(models) == 0 {
		return 0, errors.New("choose 1 to 10 models")
	}
	if models[0] == "qwen" {
		return 0, errors.Join(postgres.ErrModelPolicy, errors.New("qwen"))
	}
	store.queued = append(store.queued, append(models, suites...))
	return 1, nil
}

func TestBakeoffPages(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	store := &fakeBakeoffStore{fakeStore: fakeStore{settings: []overload.ReviewSettings{{Name: "flint", Model: "flint"}}}}
	handler := Handler(store)
	get := func(path string, status int) string {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != status {
			t.Fatalf("%s: %d", path, response.Code)
		}
		return response.Body.String()
	}
	page := get("/bakeoffs", http.StatusOK)
	if !strings.Contains(page, `value="flint"`) || !strings.Contains(page, "planted") || !strings.Contains(page, `href="/bakeoffs/1"`) {
		t.Fatal("bake-offs page is missing models, suites or history")
	}
	detail := get("/bakeoffs/1", http.StatusOK)
	if !strings.Contains(detail, "4 of 5") || !strings.Contains(detail, "model timed out") || !strings.Contains(detail, "6m0s") {
		t.Fatalf("detail page: %s", detail)
	}
	get("/bakeoffs/2", http.StatusNotFound)
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page)[1]
	post := func(form url.Values, status int) *httptest.ResponseRecorder {
		t.Helper()
		form.Set("csrf", csrf)
		request := httptest.NewRequest(http.MethodPost, "/bakeoffs", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("post %v: %d %s", form, response.Code, response.Body.String())
		}
		return response
	}
	post(url.Values{"models": {"flint"}, "suites": {"planted"}, "runs": {"x"}}, http.StatusUnprocessableEntity)
	if body := post(url.Values{"models": {"qwen"}, "suites": {"planted"}, "runs": {"2"}}, http.StatusUnprocessableEntity).Body.String(); !strings.Contains(body, "model policy") {
		t.Fatalf("policy refusal: %s", body)
	}
	if response := post(url.Values{"models": {"flint"}, "suites": {"planted"}, "runs": {"2"}}, http.StatusSeeOther); !strings.HasPrefix(response.Header().Get("Location"), "/bakeoffs/1") {
		t.Fatalf("redirect = %s", response.Header().Get("Location"))
	}
}
