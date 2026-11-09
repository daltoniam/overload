package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestDebounceAndReviewLimit(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	store.Now = func() time.Time { return now }
	name := fmt.Sprintf("rereview-%d", time.Now().UnixNano())
	repo := "test/" + name
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`,
			`UPDATE webhook_deliveries SET run_id=NULL WHERE repository_full_name=$1`,
			`DELETE FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`,
			`DELETE FROM webhook_deliveries WHERE repository_full_name=$1`,
			`DELETE FROM trigger_bindings WHERE repository_full_name=$1`,
			`DELETE FROM repositories WHERE full_name=$1`,
		} {
			_, _ = store.Pool.Exec(ctx, statement, repo)
		}
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
	// Defaults: a 5-minute debounce and 3 webhook reviews per pull request.
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "pr_review", Agents: []string{name}, Enabled: true, ReviewPolicy: overload.ReviewPolicy{Rereview: &overload.RereviewSettings{}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRepository(ctx, repo, true, true); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"opened", "synchronize"} {
		if err := store.SaveBinding(ctx, overload.TriggerBinding{Source: "github", Event: "pull_request", Action: action, Repository: repo, Workflow: name, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	push := func(action string, head byte, manual bool) (string, string, int64) {
		t.Helper()
		id := fmt.Sprintf("%s-%c-%v", name, head, manual)
		delivery := PullRequestDelivery{DeliveryID: id, Action: action, RepoName: repo, PR: 5, HeadSHA: strings.Repeat(string(head), 40), BaseSHA: strings.Repeat("0", 40), Eligible: true, Payload: []byte(fmt.Sprintf(`{"id":%q}`, id)), Manual: manual}
		if _, err := store.IngestPR(ctx, client, delivery); err != nil {
			t.Fatal(err)
		}
		source := "github"
		if manual {
			source = "manual"
		}
		outcome, reason, runID, err := store.DeliveryOutcome(ctx, source, id)
		if err != nil {
			t.Fatal(err)
		}
		return outcome, reason, runID
	}
	complete := func(runID int64) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `UPDATE runs SET status='completed' WHERE id=$1`, runID); err != nil {
			t.Fatal(err)
		}
	}
	outcome, _, opened := push("opened", 'a', false)
	if outcome != "queued" {
		t.Fatalf("opened: %s", outcome)
	}
	complete(opened)
	_, _, first := push("synchronize", 'b', false)
	var scheduled time.Time
	if err := store.Pool.QueryRow(ctx, `SELECT j.scheduled_at FROM runs r JOIN river_job j ON j.id=r.river_job_id WHERE r.id=$1`, first).Scan(&scheduled); err != nil || scheduled.Before(now.Add(4*time.Minute)) {
		t.Fatalf("push not held: scheduled %v %v", scheduled, err)
	}
	// A second push inside the window replaces the held one.
	_, _, second := push("synchronize", 'c', false)
	var status overload.RunStatus
	if err := store.Pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, first).Scan(&status); err != nil || status != overload.RunSuperseded || second == first {
		t.Fatalf("first push status %s, second run %d: %v", status, second, err)
	}
	complete(second)
	outcome, _, third := push("synchronize", 'd', false)
	if outcome != "queued" {
		t.Fatalf("third review: %s", outcome)
	}
	complete(third)
	// Three webhook reviews ran (the superseded one does not count).
	outcome, reason, _ := push("synchronize", 'e', false)
	if outcome != "skipped" || !strings.HasPrefix(reason, SkipReviewLimit) {
		t.Fatalf("fourth review: %s %s", outcome, reason)
	}
	if outcome, reason, _ := push("opened", 'e', true); outcome != "queued" {
		t.Fatalf("a review asked for by hand skips the limit: %s %s", outcome, reason)
	}
}
