package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
)

func TestConfigurationDeletionGuards(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := "delete-guard-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM schedules WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "test", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePrompt(ctx, overload.PromptTemplate{Name: name, Kind: "entry", Body: "Review safely"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Kind: "scheduled_prompt", Model: name, EntryPrompt: name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "scheduled_prompt", Agents: []string{name}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSchedule(ctx, overload.Schedule{Name: name, Workflow: name, Cron: "0 9 * * *", Timezone: "UTC", Input: []byte(`{}`), Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePrompt(ctx, "entry", name); err == nil {
		t.Fatal("deleted referenced prompt")
	}
	if err := store.DeleteWorkflow(ctx, name); err == nil {
		t.Fatal("deleted scheduled workflow")
	}
	if err := store.DeleteSchedule(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWorkflow(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAgent(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePrompt(ctx, "entry", name); err != nil {
		t.Fatal(err)
	}
}
