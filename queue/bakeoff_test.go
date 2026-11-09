package queue

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/postgres/pgtest"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestBakeoffWorker(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	for _, setting := range []overload.ReviewSettings{
		{Name: "flint", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "flint", PromptProfile: "context", Catalog: "recursive-flint"},
		{Name: "qwen", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen3.8-flash-next", PromptProfile: "context"},
	} {
		if err := store.SaveReviewSettings(ctx, setting); err != nil {
			t.Fatal(err)
		}
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBakeoff(ctx, client, []string{"nope"}, []string{eval.BuiltinSuite}, 1); err == nil {
		t.Fatal("queued a bake-off for an unknown model")
	}
	if _, err := store.CreateBakeoff(ctx, client, []string{"flint"}, []string{"nope"}, 1); err == nil {
		t.Fatal("queued a bake-off for an unknown suite")
	}
	id, err := store.CreateBakeoff(ctx, client, []string{"flint", "qwen"}, []string{eval.BuiltinSuite}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveModelPolicy(ctx, catalog.Policy{BlockedOrigins: []string{"CN"}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	worker := &BakeoffWorker{Store: store, Review: func(_ context.Context, spec overload.ReviewSpec, repo fs.FS) (overload.ReviewResult, error) {
		calls++
		if _, err := fs.Stat(repo, "billing/invoice.go"); err != nil && spec.Workflow.Agents[0].Model.Name == "flint" && calls == 1 {
			return overload.ReviewResult{}, err
		}
		if calls == 2 {
			return overload.ReviewResult{}, errors.New("model timed out")
		}
		return overload.ReviewResult{Findings: []overload.Finding{{Path: "billing/invoice.go", Line: 41, Title: "SQL injection", Body: "string concatenation"}, {Path: "util/textutil.go", Line: 15, Title: "SQL injection", Body: "string concatenation"}}, Metrics: map[string]any{"input_tokens": int64(100), "output_tokens": float64(20)}}, nil
	}}
	if err := worker.Work(ctx, &river.Job[postgres.BakeoffArgs]{Args: postgres.BakeoffArgs{ID: id}}); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("reviews = %d, want 4 (2 cases x 2 runs for the allowed model)", calls)
	}
	bakeoff, err := store.GetBakeoff(ctx, id)
	if err != nil || bakeoff.Status != "completed" || len(bakeoff.Results) != 8 {
		t.Fatalf("bake-off = %+v, %v", bakeoff, err)
	}
	refused, failed, matched := 0, 0, 0
	for _, result := range bakeoff.Results {
		switch {
		case result.Model == "qwen" && result.Error != "":
			refused++
		case result.Error != "":
			failed++
		default:
			matched += result.Score.Matched
		}
	}
	if refused != 4 || failed != 1 || matched != 3 {
		t.Fatalf("refused %d, failed %d, matched %d: %+v", refused, failed, matched, bakeoff.Results)
	}
	if err := worker.Work(ctx, &river.Job[postgres.BakeoffArgs]{Args: postgres.BakeoffArgs{ID: id}}); err != nil || calls != 4 {
		t.Fatalf("a finished bake-off ran again: %v", err)
	}
	list, err := store.ListBakeoffs(ctx)
	if err != nil || len(list) != 1 || len(list[0].Models) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE bakeoffs SET status='running'`); err != nil {
		t.Fatal(err)
	}
	if interrupted, err := store.FailInterruptedBakeoffs(ctx); err != nil || interrupted != 1 {
		t.Fatalf("interrupted = %d, %v", interrupted, err)
	}
}

func TestSavedSuites(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	suite := eval.Suite{Name: "ours", Cases: eval.PlantedSuite().Cases[1:]}
	if err := store.SaveSuite(ctx, suite); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSuite(ctx, eval.Suite{Name: eval.BuiltinSuite, Cases: suite.Cases}); err == nil {
		t.Fatal("saved over the built-in suite")
	}
	suites, err := store.ListSuites(ctx)
	if err != nil || len(suites) != 2 || suites[0].Name != eval.BuiltinSuite || suites[1].Cases[0].Name != "planted-misleading-file" {
		t.Fatalf("suites = %+v, %v", suites, err)
	}
	if err := store.DeleteSuite(ctx, "ours"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSuite(ctx, "ours"); err == nil {
		t.Fatal("deleted a missing suite")
	}
}
