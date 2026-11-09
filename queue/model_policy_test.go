package queue

import (
	"context"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/postgres/pgtest"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestReviewWorkerRefusesModelsThePolicyNowRefuses(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: "qwen", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen3.8-flash-next", PromptProfile: "context", Catalog: "qwen3.8-flash-next"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: "reviewer", Model: "qwen", Prompt: "Review the diff.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: "review", Kind: "pr_review", Agents: []string{"reviewer"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRepository(ctx, "test/policy", true, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBinding(ctx, overload.TriggerBinding{Source: "github", Event: "pull_request", Action: "opened", Repository: "test/policy", Workflow: "review", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("b", 40)
	if accepted, err := store.IngestPR(ctx, client, postgres.PullRequestDelivery{DeliveryID: "policy-1", Action: "opened", Payload: []byte(`{}`), RepoName: "test/policy", PR: 7, HeadSHA: head, BaseSHA: strings.Repeat("a", 40), Eligible: true}); err != nil || !accepted {
		t.Fatalf("ingest: %v %v", accepted, err)
	}
	var runID int64
	if err := store.Pool.QueryRow(ctx, `SELECT id FROM runs WHERE head_sha=$1`, head).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveModelPolicy(ctx, catalog.Policy{BlockedOrigins: []string{"CN"}}); err != nil {
		t.Fatal(err)
	}
	worker := &ReviewWorker{Store: store, Source: webhookSource{}}
	if err := worker.Work(ctx, &river.Job[postgres.ReviewArgs]{Args: postgres.ReviewArgs{RunID: runID}}); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil || run.Status != overload.RunFailed || run.ErrorCode != "model_policy" || !strings.Contains(run.ErrorMessage, "from CN") {
		t.Fatalf("run = %+v, %v", run, err)
	}
}
