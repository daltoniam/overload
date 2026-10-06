package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestScheduleEnqueueDeduplicates(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM job_outputs WHERE run_id IN (SELECT id FROM runs WHERE schedule_id IN (SELECT id FROM schedules WHERE name='test-schedule-configuration'))`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM schedule_occurrences WHERE schedule_id IN (SELECT id FROM schedules WHERE name='test-schedule-configuration')`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM runs WHERE schedule_id IN (SELECT id FROM schedules WHERE name='test-schedule-configuration')`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM schedules WHERE name='test-schedule-configuration'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM workflows WHERE name='test-scheduled-workflow'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM agent_definitions WHERE name='test-scheduled-agent'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name='agent:test-scheduled-agent')`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM prompt_templates WHERE name='agent:test-scheduled-agent'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE name='test-scheduled-model'`)
		store.Pool.Close()
	})
	model := overload.ReviewSettings{Name: "test-scheduled-model", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "test", PromptProfile: "context"}
	if err := store.SaveReviewSettings(ctx, model); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: "test-scheduled-agent", Kind: "scheduled_prompt", Model: model.Name, Prompt: "Summarize input.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, agent := range agents {
		if agent.Name == "test-scheduled-agent" && agent.Kind == "scheduled_prompt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("scheduled agent type missing: %+v", agents)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: "wrong-kind-workflow", Kind: "pr_review", Agents: []string{"test-scheduled-agent"}, Enabled: true}); err == nil {
		t.Fatal("scheduled agent accepted by PR workflow")
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: "test-scheduled-workflow", Kind: "scheduled_prompt", Agents: []string{"test-scheduled-agent"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	schedule := overload.Schedule{Name: "test-schedule-configuration", Workflow: "test-scheduled-workflow", Cron: "* * * * *", Timezone: "UTC", Input: []byte(`{"topic":"status"}`), Enabled: true}
	if err := store.SaveSchedule(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE schedules SET next_run_at=$2 WHERE name=$1`, schedule.Name, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	count, err := store.EnqueueDueSchedules(ctx, client)
	if err != nil || count < 1 {
		t.Fatalf("first scan: %d %v", count, err)
	}
	count, err = store.EnqueueDueSchedules(ctx, client)
	if err != nil || count != 0 {
		t.Fatalf("duplicate scan: %d %v", count, err)
	}
	client2, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	count, err = store.EnqueueDueSchedules(ctx, client2)
	if err != nil || count != 0 {
		t.Fatalf("second scanner duplicated run: %d %v", count, err)
	}
	var jobs int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM schedule_occurrences WHERE schedule_id=(SELECT id FROM schedules WHERE name=$1)`, schedule.Name).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("occurrences: %d %v", jobs, err)
	}
	var frozen []byte
	if err := store.Pool.QueryRow(ctx, `SELECT input_snapshot FROM runs WHERE schedule_id=(SELECT id FROM schedules WHERE name=$1) ORDER BY id DESC LIMIT 1`, schedule.Name).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(frozen, &decoded); err != nil || decoded["topic"] != "status" {
		t.Fatalf("input snapshot: %s %v", frozen, err)
	}
}
