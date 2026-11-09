package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/postgres/pgtest"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestModelPolicy(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	policy, err := store.LoadModelPolicy(ctx)
	if err != nil || policy.Restrictive() {
		t.Fatalf("default policy = %+v, %v", policy, err)
	}
	qwen := overload.ReviewSettings{Name: "qwen", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen3.8-flash-next", PromptProfile: "context", Catalog: "qwen3.8-flash-next"}
	if err := store.SaveReviewSettings(ctx, qwen); err != nil {
		t.Fatal(err)
	}
	unlabelled := overload.ReviewSettings{Name: "unlabelled", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8001/v1", Model: "qwen3.8-flash-next", PromptProfile: "context"}
	if err := store.SaveReviewSettings(ctx, unlabelled); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetReviewSettings(ctx, "qwen")
	if err != nil || loaded.Catalog != "qwen3.8-flash-next" {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	for _, agent := range []overload.AgentDefinition{{Name: "reviewer", Model: "qwen", Prompt: "Review the diff.", Enabled: true}, {Name: "summarizer", Kind: "scheduled_prompt", Model: "unlabelled", Prompt: "Summarize.", Enabled: true}} {
		if err := store.SaveAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: "review", Kind: "pr_review", Agents: []string{"reviewer"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: "summary", Kind: "scheduled_prompt", Agents: []string{"summarizer"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	schedule := overload.Schedule{Name: "daily", Workflow: "summary", Cron: "0 9 * * *", Timezone: "UTC", Enabled: true}
	if err := store.SaveSchedule(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	pinned, err := store.ResolveRunnableWorkflow(ctx, "review")
	if err != nil {
		t.Fatalf("resolve before policy: %v", err)
	}

	if err := store.SaveModelPolicy(ctx, catalog.Policy{BlockedOrigins: []string{"cn"}}); err != nil {
		t.Fatal(err)
	}
	policy, err = store.LoadModelPolicy(ctx)
	if err != nil || strings.Join(policy.BlockedOrigins, ",") != "CN" {
		t.Fatalf("saved policy = %+v, %v", policy, err)
	}
	if _, err := store.ResolveWorkflow(ctx, "review"); err != nil {
		t.Fatalf("reading a refused workflow for display: %v", err)
	}
	_, err = store.ResolveRunnableWorkflow(ctx, "review")
	if !errors.Is(err, postgres.ErrWorkflowUnavailable) || !strings.Contains(err.Error(), "model not allowed by the model policy: model qwen of agent reviewer: its publisher is from CN") {
		t.Fatalf("resolve after policy: %v", err)
	}
	if _, err := store.ResolveRunnableWorkflow(ctx, "summary"); !errors.Is(err, postgres.ErrWorkflowUnavailable) {
		t.Fatalf("a connection without a catalog entry was not identified by its model ID: %v", err)
	}
	if err := store.CheckModelPolicy(ctx, pinned); !errors.Is(err, postgres.ErrModelPolicy) {
		t.Fatalf("a run pinned before the policy changed was not refused: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE schedules SET next_run_at=now()-interval '1 minute' WHERE name='daily'`); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		count, err := store.EnqueueDueSchedules(ctx, client)
		if err != nil || count != 0 {
			t.Fatalf("refused schedule: queued %d, %v", count, err)
		}
		if _, err := store.Pool.Exec(ctx, `UPDATE schedules SET next_run_at=now()-interval '1 minute' WHERE name='daily'`); err != nil {
			t.Fatal(err)
		}
	}
	var failed int
	var enabled bool
	if err := store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM runs WHERE schedule_id=s.id AND status='failed'), s.enabled FROM schedules s WHERE name='daily'`).Scan(&failed, &enabled); err != nil || failed != 1 || enabled {
		t.Fatalf("refused schedule: %d failed runs, enabled %v, %v", failed, enabled, err)
	}
	var next time.Time
	if err := store.Pool.QueryRow(ctx, `SELECT next_run_at FROM schedules WHERE name='daily'`).Scan(&next); err != nil || next.IsZero() {
		t.Fatalf("refused schedule was not moved to its next time: %v %v", next, err)
	}
	var code, message string
	if err := store.Pool.QueryRow(ctx, `SELECT r.error_code, r.error_message FROM schedule_occurrences o JOIN runs r ON r.id=o.run_id WHERE o.schedule_id=(SELECT id FROM schedules WHERE name='daily') AND r.status='failed'`).Scan(&code, &message); err != nil || code != "model_policy" || !strings.Contains(message, "Schedule paused") || !strings.Contains(message, "from CN") {
		t.Fatalf("skipped occurrence not recorded: %q %q %v", code, message, err)
	}
	schedule.Enabled = false
	if err := store.SaveSchedule(ctx, schedule); err != nil {
		t.Fatalf("pausing a refused schedule: %v", err)
	}
	schedule.Enabled = true
	if err := store.SaveSchedule(ctx, schedule); !errors.Is(err, postgres.ErrWorkflowUnavailable) {
		t.Fatalf("enabling a refused schedule: %v", err)
	}
	if _, err := store.RunScheduleNow(ctx, client, "daily"); !errors.Is(err, postgres.ErrWorkflowUnavailable) {
		t.Fatalf("run now with a refused model: %v", err)
	}

	qwen.Concurrency = 2
	if err := store.SaveReviewSettings(ctx, qwen); !errors.Is(err, postgres.ErrModelPolicy) {
		t.Fatalf("saving a refused model: %v", err)
	}
	mislabelled := overload.ReviewSettings{Name: "sneaky", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen3.8-flash-next", PromptProfile: "context", Catalog: "recursive-flint"}
	if err := store.SaveReviewSettings(ctx, mislabelled); err == nil || !strings.Contains(err.Error(), "is catalog entry qwen3.8-flash-next") {
		t.Fatalf("saving a mislabelled model: %v", err)
	}
	flint := overload.ReviewSettings{Name: "flint", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "flint", PromptProfile: "context", Catalog: "recursive-flint"}
	if err := store.SaveReviewSettings(ctx, flint); err != nil {
		t.Fatalf("saving an allowed model: %v", err)
	}
	if err := store.SaveModelPolicy(ctx, catalog.Policy{AllowedOrigins: []string{"US"}, BlockedOrigins: []string{"US"}}); err == nil {
		t.Fatal("saved a contradictory policy")
	}
	if err := store.SaveModelPolicy(ctx, catalog.Policy{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveRunnableWorkflow(ctx, "review"); err != nil {
		t.Fatalf("resolve after clearing policy: %v", err)
	}
}
