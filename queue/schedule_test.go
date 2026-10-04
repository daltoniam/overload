package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestScheduleWorkerFrozenInputAndFailure(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("schedule-worker-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM job_outputs WHERE run_id IN (SELECT id FROM runs WHERE schedule_id IN (SELECT id FROM schedules WHERE name=$1))`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM schedule_occurrences WHERE schedule_id IN (SELECT id FROM schedules WHERE name=$1)`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE schedule_id IN (SELECT id FROM schedules WHERE name=$1)`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM schedules WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
			MaxTokens          int            `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.ChatTemplateKwargs["reasoning_effort"] != "xhigh" || request.MaxTokens <= 1024 {
			t.Errorf("scheduled prompt ignored the model's reasoning settings: %v max_tokens=%d", request.ChatTemplateKwargs, request.MaxTokens)
		}
		text := fmt.Sprint(request.Messages)
		if !strings.Contains(text, "Pinned entry") || !strings.Contains(text, "Pinned review") || !strings.Contains(text, `"topic": "original"`) && !strings.Contains(text, `"topic":"original"`) || strings.Contains(text, "changed") {
			t.Errorf("unexpected model request: %s", text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Scheduled result"}}]}`)
	}))
	defer model.Close()
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: model.URL, Model: "test", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePrompt(ctx, overload.PromptTemplate{Name: name, Kind: "entry", Body: "Pinned entry"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePrompt(ctx, overload.PromptTemplate{Name: name, Kind: "review", Body: "Pinned review"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Kind: "scheduled_prompt", Model: name, EntryPrompt: name, ReviewPrompt: name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "scheduled_prompt", Agents: []string{name}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSchedule(ctx, overload.Schedule{Name: name, Workflow: name, Cron: "* * * * *", Timezone: "UTC", Input: []byte(`{"topic":"original"}`), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE schedules SET next_run_at=now()-interval '1 minute' WHERE name=$1`, name); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if count, err := store.EnqueueDueSchedules(ctx, client); err != nil || count < 1 {
		t.Fatalf("enqueue: %d %v", count, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE schedules SET input_json='{"topic":"changed"}'::jsonb WHERE name=$1`, name); err != nil {
		t.Fatal(err)
	}
	var runID int64
	if err := store.Pool.QueryRow(ctx, `SELECT id FROM runs WHERE schedule_id=(SELECT id FROM schedules WHERE name=$1)`, name).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	worker := &ScheduleWorker{Store: store}
	job := &river.Job[postgres.ScheduleArgs]{Args: postgres.ScheduleArgs{RunID: runID}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil || run.Status != "completed" || calls != 1 {
		t.Fatalf("run=%+v calls=%d err=%v", run, calls, err)
	}
	var kind string
	var content []byte
	if err := store.Pool.QueryRow(ctx, `SELECT kind,content FROM job_outputs WHERE run_id=$1`, runID).Scan(&kind, &content); err != nil || kind != "scheduled_prompt" || !strings.Contains(string(content), "Scheduled result") || !strings.Contains(string(content), name) {
		t.Fatalf("output kind=%q content=%s err=%v", kind, content, err)
	}
	if err := worker.Work(ctx, job); err != nil || calls != 1 {
		t.Fatalf("duplicate work calls=%d err=%v", calls, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET status='queued',config_snapshot=jsonb_set(config_snapshot,'{agents,0,entry_prompt,sha256}','"invalid"') WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	run, err = store.GetRun(ctx, runID)
	if err != nil || run.Status != "failed" || calls != 1 {
		t.Fatalf("tampered run=%+v calls=%d err=%v", run, calls, err)
	}
}
