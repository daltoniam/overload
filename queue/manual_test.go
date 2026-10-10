package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	gh "github.com/google/go-github/v75/github"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

type fakePRReader struct {
	pr    *gh.PullRequest
	calls int
}

func (reader *fakePRReader) PullRequest(_ context.Context, installationID int64, _ string, _ int) (*gh.PullRequest, string, error) {
	reader.calls++
	if installationID < 1 {
		return nil, "", fmt.Errorf("no installation")
	}
	return reader.pr, "", nil
}

func TestQueueManualReview(t *testing.T) {
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
	id := time.Now().UnixNano()
	installationID := id % 1_000_000_000_000
	name := fmt.Sprintf("manual-%d", id)
	repo := "test-" + name + "/api"
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE repository_full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM trigger_bindings WHERE repository_full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM github_installations WHERE id=$1`, installationID)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: "http://127.0.0.1:1/v1", Model: "m", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Model: name, Prompt: "Review.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "pr_review", Agents: []string{name}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO github_installations (id, account_login, account_type) VALUES ($1,'acme','Organization')`, installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO repositories (full_name, enabled, dry_run, installation_id, github_id) VALUES ($1, false, false, $2, $3)`, repo, installationID, id%1_000_000_000); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("c", 40)
	reader := &fakePRReader{pr: &gh.PullRequest{Number: gh.Ptr(296), State: gh.Ptr("open"), Head: &gh.PullRequestBranch{SHA: gh.Ptr(head)}, Base: &gh.PullRequestBranch{SHA: gh.Ptr(strings.Repeat("a", 40)), Repo: &gh.Repository{ID: gh.Ptr(id % 1_000_000_000)}}}}
	queue := func() (int64, error) { return QueueManualReview(ctx, store, client, reader, repo, 296, false) }

	if _, err := queue(); !errors.Is(err, ErrReviewNotQueued) || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("disabled repository: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE repositories SET enabled=true WHERE full_name=$1`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := queue(); !errors.Is(err, ErrReviewNotQueued) || !strings.Contains(err.Error(), "no matching workflow binding") {
		t.Fatalf("no binding: %v", err)
	}
	if err := store.SaveBinding(ctx, overload.TriggerBinding{Source: "github", Event: "pull_request", Action: "opened", Repository: repo, Workflow: name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runID, err := queue()
	if err != nil || runID == 0 {
		t.Fatalf("queue: %d %v", runID, err)
	}
	var trigger, status, sha string
	if err := store.Pool.QueryRow(ctx, `SELECT trigger, status, head_sha FROM runs WHERE id=$1`, runID).Scan(&trigger, &status, &sha); err != nil || trigger != "manual" || status != "queued" || sha != head {
		t.Fatalf("run trigger=%q status=%q sha=%q err=%v", trigger, status, sha, err)
	}
	again, err := queue()
	if !errors.Is(err, ErrReviewNotQueued) || again != runID || !strings.Contains(err.Error(), "already has run") {
		t.Fatalf("second request for the same head: %d %v", again, err)
	}
	if _, err := QueueManualReview(ctx, store, client, reader, repo, 296, true); !errors.Is(err, ErrReviewNotQueued) || !strings.Contains(err.Error(), "still queued or running") {
		t.Fatalf("again while the first is queued: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET status='completed' WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	second, err := QueueManualReview(ctx, store, client, reader, repo, 296, true)
	if err != nil || second == runID || second == 0 {
		t.Fatalf("review again after completion: %d %v", second, err)
	}
	reader.pr.Draft = gh.Ptr(true)
	if _, err := queue(); !errors.Is(err, ErrReviewNotQueued) || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("draft: %v", err)
	}
	reader.pr.Draft, reader.pr.State = nil, gh.Ptr("closed")
	if _, err := queue(); !errors.Is(err, ErrReviewNotQueued) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed: %v", err)
	}
	if _, err := QueueManualReview(ctx, store, client, reader, "nobody/nothing", 1, false); !errors.Is(err, ErrReviewNotQueued) {
		t.Fatalf("unknown repository: %v", err)
	}
}
