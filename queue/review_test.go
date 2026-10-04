package queue

import (
	"context"
	"os"
	"testing"

	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
)

func TestReviewWorkerTransitions(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRepository(ctx, "test/worker", false, true); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, trigger) VALUES ((SELECT id FROM repositories WHERE full_name = 'test/worker'), 1, 'test') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	worker := &ReviewWorker{Store: store}
	if err := worker.Work(ctx, &river.Job[postgres.ReviewArgs]{Args: postgres.ReviewArgs{RunID: id}}); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, id)
	if err != nil || run.Status != "failed" || run.ErrorCode != "invalid_workflow" {
		t.Fatalf("run: %+v %v", run, err)
	}
	events, err := store.ListRunEvents(ctx, id)
	if err != nil || len(events) != 1 {
		t.Fatalf("events: %+v %v", events, err)
	}
	if err := worker.Work(ctx, &river.Job[postgres.ReviewArgs]{Args: postgres.ReviewArgs{RunID: id}}); err != nil {
		t.Fatal("duplicate run", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET status='running',started_at=now(),error_code='',error_message='' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(ctx, &river.Job[postgres.ReviewArgs]{Args: postgres.ReviewArgs{RunID: id}}); err != nil {
		t.Fatal("interrupted run", err)
	}
	run, err = store.GetRun(ctx, id)
	if err != nil || run.Status != "failed" || run.ErrorCode != "review_interrupted" {
		t.Fatalf("interrupted run: %+v %v", run, err)
	}
}
