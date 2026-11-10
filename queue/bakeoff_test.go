package queue

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/postgres/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func bakeoffStore(t *testing.T) (*postgres.Store, *river.Client[pgx.Tx]) {
	t.Helper()
	store := pgtest.Isolated(t)
	for _, setting := range []overload.ReviewSettings{
		{Name: "flint", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "flint", PromptProfile: "switchboard-go", Catalog: "recursive-flint"},
		{Name: "qwen", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen3.8-flash-next", PromptProfile: "context"},
	} {
		if err := store.SaveReviewSettings(context.Background(), setting); err != nil {
			t.Fatal(err)
		}
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return store, client
}

func plantedFindings() []overload.Finding {
	return []overload.Finding{{Path: "billing/invoice.go", Line: 41, Title: "SQL injection", Body: "string concatenation"}, {Path: "util/textutil.go", Line: 15, Title: "SQL injection", Body: "string concatenation"}}
}

func TestBakeoffWorker(t *testing.T) {
	store, client := bakeoffStore(t)
	ctx := context.Background()
	if _, err := store.CreateBakeoff(ctx, client, []string{"nope"}, []string{eval.BuiltinSuite}, 1); err == nil {
		t.Fatal("queued a bake-off for an unknown model")
	}
	if _, err := store.CreateBakeoff(ctx, client, []string{"flint"}, []string{"nope"}, 1); err == nil {
		t.Fatal("queued a bake-off for an unknown suite")
	}
	id, err := store.CreateBakeoff(ctx, client, []string{"flint", " qwen", "flint"}, []string{eval.BuiltinSuite, eval.BuiltinSuite + " "}, 2)
	if err != nil {
		t.Fatal(err)
	}
	var queue string
	var attempts int
	if err := store.Pool.QueryRow(ctx, `SELECT queue, max_attempts FROM river_job WHERE kind = 'bakeoff'`).Scan(&queue, &attempts); err != nil || queue != postgres.BakeoffQueue || attempts < 2 {
		t.Fatalf("job queue %q with %d attempts, %v", queue, attempts, err)
	}
	if err := store.SaveModelPolicy(ctx, catalog.Policy{BlockedOrigins: []string{"CN"}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	worker := &BakeoffWorker{Store: store, Review: func(_ context.Context, spec overload.ReviewSpec, repo fs.FS) (overload.ReviewResult, error) {
		calls++
		if spec.Workflow.Name != "switchboard-go-v1" {
			return overload.ReviewResult{}, fmt.Errorf("reviewed with %s, not the connection's prompt", spec.Workflow.Name)
		}
		if _, err := fs.Stat(repo, "billing/invoice.go"); err != nil && calls == 1 {
			return overload.ReviewResult{}, err
		}
		switch calls {
		case 2:
			return overload.ReviewResult{}, errors.New("model timed out")
		case 3:
			return overload.ReviewResult{Findings: plantedFindings(), Metrics: map[string]any{"degraded": true, "routing": overload.Routing{Degraded: []string{"1 files were too large to review: util/textutil.go"}}}}, nil
		}
		return overload.ReviewResult{Findings: plantedFindings(), Metrics: map[string]any{"input_tokens": int64(100), "output_tokens": float64(20)}}, nil
	}}
	if err := worker.Work(ctx, &river.Job[postgres.BakeoffArgs]{Args: postgres.BakeoffArgs{ID: id}}); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("reviews = %d, want 4 (2 cases x 2 runs for the allowed model)", calls)
	}
	bakeoff, err := store.GetBakeoff(ctx, id)
	if err != nil || bakeoff.Status != "completed" || len(bakeoff.Results) != 8 || !slices.Equal(bakeoff.SuiteNames, []string{eval.BuiltinSuite}) {
		t.Fatalf("bake-off = %+v, %v", bakeoff, err)
	}
	refused, matched := 0, 0
	var failures []string
	for _, result := range bakeoff.Results {
		switch {
		case result.Model == "qwen" && result.Error != "":
			refused++
		case result.Error != "":
			failures = append(failures, result.Error)
		default:
			matched += result.Score.Matched
			if result.Score.PlacementChecked != result.Score.Expected || result.Score.Unmatched != 1 {
				t.Errorf("score = %+v", result.Score)
			}
		}
	}
	if refused != 4 || matched != 2 || len(failures) != 2 || failures[0] != "model timed out" || !strings.Contains(failures[1], "review incomplete: 1 files were too large") {
		t.Fatalf("refused %d, matched %d, failures %q", refused, matched, failures)
	}
	if err := worker.Work(ctx, &river.Job[postgres.BakeoffArgs]{Args: postgres.BakeoffArgs{ID: id}}); err != nil || calls != 4 {
		t.Fatalf("a finished bake-off ran again: %v", err)
	}
	list, err := store.ListBakeoffs(ctx)
	if err != nil || len(list) != 1 || len(list[0].Models) != 2 || list[0].Models[0].Prompt() != "switchboard-go" || list[0].Models[1].Prompt() != "context" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE bakeoffs SET status='running'`); err != nil {
		t.Fatal(err)
	}
	if interrupted, err := store.FailInterruptedBakeoffs(ctx); err != nil || interrupted != 1 {
		t.Fatalf("interrupted = %d, %v", interrupted, err)
	}
}

func TestBakeoffWorkerChecksPolicyBeforeEachReview(t *testing.T) {
	store, client := bakeoffStore(t)
	ctx := context.Background()
	id, err := store.CreateBakeoff(ctx, client, []string{"flint"}, []string{eval.BuiltinSuite}, 1)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	worker := &BakeoffWorker{Store: store, Review: func(context.Context, overload.ReviewSpec, fs.FS) (overload.ReviewResult, error) {
		calls++
		return overload.ReviewResult{}, store.SaveModelPolicy(ctx, catalog.Policy{BlockedOrigins: []string{"US"}})
	}}
	if err := worker.Work(ctx, &river.Job[postgres.BakeoffArgs]{Args: postgres.BakeoffArgs{ID: id}}); err != nil {
		t.Fatal(err)
	}
	bakeoff, err := store.GetBakeoff(ctx, id)
	if err != nil || calls != 1 || len(bakeoff.Results) != 2 || !strings.HasPrefix(bakeoff.Results[1].Error, "refused by the model policy") {
		t.Fatalf("calls %d, bake-off %+v, %v", calls, bakeoff, err)
	}
}

func TestBakeoffWorkerInterrupted(t *testing.T) {
	store, client := bakeoffStore(t)
	id, err := store.CreateBakeoff(context.Background(), client, []string{"flint"}, []string{eval.BuiltinSuite}, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := &BakeoffWorker{Store: store, Review: func(reviewCtx context.Context, _ overload.ReviewSpec, _ fs.FS) (overload.ReviewResult, error) {
		cancel()
		return overload.ReviewResult{}, reviewCtx.Err()
	}}
	if err := worker.Work(ctx, &river.Job[postgres.BakeoffArgs]{Args: postgres.BakeoffArgs{ID: id}}); err != nil {
		t.Fatal(err)
	}
	bakeoff, err := store.GetBakeoff(context.Background(), id)
	if err != nil || bakeoff.Status != "failed" || bakeoff.Error != postgres.BakeoffInterrupted || len(bakeoff.Results) != 0 {
		t.Fatalf("bake-off = %+v, %v", bakeoff, err)
	}
}

func TestBakeoffLimits(t *testing.T) {
	store, client := bakeoffStore(t)
	ctx := context.Background()
	textutil := eval.PlantedSuite().Cases[1]
	large := eval.Suite{Name: "large"}
	for index := range eval.MaxCases {
		item := textutil
		item.Name = fmt.Sprintf("case-%d", index)
		large.Cases = append(large.Cases, item)
	}
	if err := store.SaveSuite(ctx, large); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		models []string
		suites []string
		runs   int
		want   string
	}{
		{"too many reviews", []string{"flint"}, []string{"large"}, postgres.MaxBakeoffRuns, "reviews"},
		{"no runs", []string{"flint"}, []string{eval.BuiltinSuite}, 0, "runs"},
		{"blank names", []string{" "}, []string{eval.BuiltinSuite}, 1, "models"},
	} {
		if _, err := store.CreateBakeoff(ctx, client, test.models, test.suites, test.runs); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v", test.name, err)
		}
	}
	if _, err := store.CreateBakeoff(ctx, client, []string{"flint"}, []string{"large"}, 1); err != nil {
		t.Fatal(err)
	}
}

func TestSavedSuites(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	suite := eval.Suite{Name: "ours", Description: "real bugs", Cases: eval.PlantedSuite().Cases[1:]}
	if err := store.SaveSuite(ctx, suite); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSuite(ctx, eval.Suite{Name: eval.BuiltinSuite, Cases: suite.Cases}); err == nil {
		t.Fatal("saved over the built-in suite")
	}
	suites, err := store.ListSuites(ctx)
	if err != nil || !slices.Equal(suites, []postgres.SuiteSummary{{Name: eval.BuiltinSuite, Description: eval.PlantedSuite().Description, Cases: 2}, {Name: "ours", Description: "real bugs", Cases: 1}}) {
		t.Fatalf("suites = %+v, %v", suites, err)
	}
	if err := store.DeleteSuite(ctx, "ours"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSuite(ctx, "ours"); err == nil {
		t.Fatal("deleted a missing suite")
	}
}
