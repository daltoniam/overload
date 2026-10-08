package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+name)
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
		if !strings.Contains(text, "Pinned entry") || !strings.Contains(text, `"topic": "original"`) && !strings.Contains(text, `"topic":"original"`) || strings.Contains(text, "changed") {
			t.Errorf("unexpected model request: %s", text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Scheduled result"}}]}`)
	}))
	defer model.Close()
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: model.URL, Model: "test", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Kind: "scheduled_prompt", Model: name, Prompt: "Pinned entry", Enabled: true}); err != nil {
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

func TestScheduleWorkerRunsToolAgent(t *testing.T) {
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
	name := fmt.Sprintf("tool-agent-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE schedule_id IN (SELECT id FROM schedules WHERE name=$1)`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM schedules WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM tool_servers WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	t.Setenv("OVERLOAD_TOOL_QUEUE_TEST", "sb_queue")
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "sb", Version: "1"}, nil)
	type executeInput struct {
		ToolName string `json:"tool_name"`
	}
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "execute", Description: "Execute a tool."}, func(ctx context.Context, request *mcp.CallToolRequest, input executeInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "issue 240: 3 candidates ranked"}}}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	tools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sb_queue" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer tools.Close()
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"`+name+`_execute","arguments":"{\"tool_name\":\"github_get_issue\"}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"2","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Updated #240."}}],"usage":{"prompt_tokens":200,"completion_tokens":20}}`)
	}))
	defer model.Close()
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: model.URL, Model: "test", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	agent := overload.AgentDefinition{Name: name, Kind: "scheduled_prompt", Model: name, Prompt: "Research integrations.", Enabled: true, Tools: []string{name}}
	if err := store.SaveAgent(ctx, agent); err == nil {
		t.Fatal("agent saved with a missing tool server")
	}
	if err := store.SaveToolServer(ctx, overload.ToolServer{Name: name, URL: tools.URL, TokenEnv: "OVERLOAD_TOOL_QUEUE_TEST", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range agents {
		if listed.Name == name && (len(listed.Tools) != 1 || listed.Tools[0] != name) {
			t.Fatalf("listed tools %v", listed.Tools)
		}
	}
	if err := store.DeleteToolServer(ctx, name); !errors.Is(err, postgres.ErrToolServerInUse) {
		t.Fatalf("deleting a used tool server: %v", err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "scheduled_prompt", Agents: []string{name}, Enabled: true, MaxSteps: 5, TimeoutMinutes: 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSchedule(ctx, overload.Schedule{Name: name, Workflow: name, Cron: "0 7 * * *", Timezone: "America/Chicago", Input: []byte(`{"repo":"daltoniam/switchboard"}`), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := store.RunScheduleNow(ctx, client, name)
	if err != nil {
		t.Fatal(err)
	}
	worker := &ScheduleWorker{Store: store}
	if err := worker.Work(ctx, &river.Job[postgres.ScheduleArgs]{Args: postgres.ScheduleArgs{RunID: runID}}); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil || run.Status != "completed" || run.Trigger != "manual" {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	output, err := store.ReadJobOutput(ctx, runID)
	if err != nil || len(output.Agents) != 1 || output.Agents[0].Text != "Updated #240." || output.Agents[0].ToolCalls != 1 || output.Agents[0].Steps != 2 || output.Agents[0].InputTokens != 300 {
		t.Fatalf("output=%+v err=%v", output, err)
	}
	events, err := store.ListRunEvents(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var toolEvent *postgres.RunEvent
	for index := range events {
		if events[index].Step == "tool" {
			toolEvent = &events[index]
		}
	}
	if toolEvent == nil || !strings.Contains(toolEvent.Message, name+"/execute") || !strings.Contains(toolEvent.Input, "github_get_issue") || toolEvent.Output != "issue 240: 3 candidates ranked" {
		t.Fatalf("tool event %+v in %+v", toolEvent, events)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE tool_servers SET enabled=false WHERE name=$1`, name); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunScheduleNow(ctx, client, name); !errors.Is(err, postgres.ErrWorkflowUnavailable) {
		t.Fatalf("run with a disabled tool server: %v", err)
	}
}
