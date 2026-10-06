package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestIngestPRTransaction(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var applied int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM overload_schema_migrations WHERE version IN ('002_review_settings.sql', '003_review_agents.sql')`).Scan(&applied); err != nil || applied != 2 {
		t.Fatalf("migration history missing: %d %v", applied, err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRepository(ctx, "test/transaction", false, true); err != nil {
		t.Fatal(err)
	}
	var repoID int64
	if err := store.Pool.QueryRow(ctx, `SELECT id FROM repositories WHERE full_name = 'test/transaction'`).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	id := time.Now().UnixNano()
	deliveryID := fmt.Sprintf("test-delivery-%d", id)
	inserted, err := store.RecordDelivery(ctx, deliveryID, "ping", "", "test/transaction", []byte(`{}`))
	if err != nil || !inserted {
		t.Fatalf("first delivery: %v, %v", inserted, err)
	}
	inserted, err = store.RecordDelivery(ctx, deliveryID, "ping", "", "test/transaction", []byte(`{}`))
	if err != nil || inserted {
		t.Fatalf("duplicate delivery: %v, %v", inserted, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE repositories SET enabled = true WHERE full_name = 'test/transaction'`); err != nil {
		t.Fatal(err)
	}
	before := countRuns(t, store)
	unmatched, err := store.IngestPR(ctx, client, PullRequestDelivery{DeliveryID: fmt.Sprintf("unbound-%d", id), Action: "opened", Payload: []byte(`{}`), RepoName: "test/unbound-transaction", PR: 42, HeadSHA: "unbound-head", BaseSHA: "def", Eligible: true})
	if err != nil || !unmatched || countRuns(t, store) != before {
		t.Fatalf("unbound event dispatched: %v %v", unmatched, err)
	}
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: "test-ingest-model", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8081/v1", Model: "test", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: "test-ingest-agent", Model: "test-ingest-model", Prompt: "Review PR.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: "test-ingest-workflow", Kind: "pr_review", Agents: []string{"test-ingest-agent"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"opened", "synchronize"} {
		if err := store.SaveBinding(ctx, overload.TriggerBinding{Source: "github", Event: "pull_request", Action: action, Repository: "test/transaction", Workflow: "test-ingest-workflow", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	headSHA := fmt.Sprintf("head-%d", id)
	inserted, err = store.IngestPR(ctx, client, PullRequestDelivery{DeliveryID: fmt.Sprintf("pr-delivery-%d", id), Action: "opened", Payload: []byte(`{}`), RepoName: "test/transaction", PR: 42, HeadSHA: headSHA, BaseSHA: "def", Eligible: true})
	if err != nil || !inserted {
		t.Fatalf("ingest: %v, %v", inserted, err)
	}
	if got := countRuns(t, store); got != before+1 {
		t.Fatalf("ingest did not create run: %d to %d", before, got)
	}
	var snapshot []byte
	if err := store.Pool.QueryRow(ctx, `SELECT config_snapshot FROM runs WHERE head_sha=$1 AND repository_id=$2`, headSHA, repoID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var resolved overload.ResolvedWorkflow
	if err := json.Unmarshal(snapshot, &resolved); err != nil || resolved.Name != "test-ingest-workflow" || len(resolved.Agents) != 1 {
		t.Fatalf("pinned workflow: %+v %v", resolved, err)
	}
	inserted, err = store.IngestPR(ctx, client, PullRequestDelivery{DeliveryID: fmt.Sprintf("pr-delivery-%d", id), Action: "opened", Payload: []byte(`{}`), RepoName: "test/transaction", PR: 42, HeadSHA: headSHA, BaseSHA: "def", Eligible: true})
	if err != nil || inserted {
		t.Fatalf("duplicate ingest: %v, %v", inserted, err)
	}
	if got := countRuns(t, store); got != before+1 {
		t.Fatalf("duplicate created run: %d to %d", before, got)
	}
	inserted, err = store.IngestPR(ctx, client, PullRequestDelivery{DeliveryID: fmt.Sprintf("other-delivery-%d", id), Action: "synchronize", Payload: []byte(`{}`), RepoName: "test/transaction", PR: 42, HeadSHA: headSHA, BaseSHA: "def", Eligible: true})
	if err != nil || !inserted {
		t.Fatalf("new delivery for same head: %v, %v", inserted, err)
	}
	if got := countRuns(t, store); got != before+1 {
		t.Fatalf("same head created run: %d to %d", before, got)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE repositories SET dry_run = false WHERE full_name = 'test/transaction'`); err != nil {
		t.Fatal(err)
	}
	inserted, err = store.IngestPR(ctx, client, PullRequestDelivery{DeliveryID: fmt.Sprintf("new-head-%d", id), Action: "synchronize", Payload: []byte(`{}`), RepoName: "test/transaction", PR: 42, HeadSHA: headSHA + "-new", BaseSHA: "def", Eligible: true})
	if err != nil || !inserted {
		t.Fatalf("new head ingest: %v, %v", inserted, err)
	}
	var status, jobState string
	if err := store.Pool.QueryRow(ctx, `SELECT runs.status, river_job.state FROM runs JOIN river_job ON river_job.id = runs.river_job_id WHERE runs.head_sha = $1 AND runs.repository_id = $2`, headSHA, repoID).Scan(&status, &jobState); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" || jobState != "cancelled" {
		t.Fatalf("old head: status=%s job=%s", status, jobState)
	}
	var newHeadDryRun bool
	if err := store.Pool.QueryRow(ctx, `SELECT dry_run FROM runs WHERE head_sha = $1 AND repository_id = $2`, headSHA+"-new", repoID).Scan(&newHeadDryRun); err != nil || newHeadDryRun {
		t.Fatalf("repository opted into posting but run is dry-run: %v %v", newHeadDryRun, err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(context.Background(), `UPDATE runs SET binding_id=NULL WHERE binding_id IN (SELECT id FROM trigger_bindings WHERE repository_full_name='test/transaction')`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM trigger_bindings WHERE repository_full_name='test/transaction'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM workflows WHERE name='test-ingest-workflow'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM agent_definitions WHERE name='test-ingest-agent'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name='agent:test-ingest-agent')`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM prompt_templates WHERE name='agent:test-ingest-agent'`)
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE name='test-ingest-model'`)
		store.Pool.Close()
	})
}

func countRuns(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	if err := store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM runs r JOIN repositories repo ON repo.id = r.repository_id WHERE repo.full_name IN ('test/transaction', 'test/unbound-transaction')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
