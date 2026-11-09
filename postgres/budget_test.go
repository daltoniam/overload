package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func TestBudgetSkipsDeliveriesWhenSpent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2099, 5, 20, 12, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	name := fmt.Sprintf("budget-%d", time.Now().UnixNano())
	repo := "test/" + name
	installation := time.Now().UnixNano() % 1_000_000_000_000
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`,
			`UPDATE webhook_deliveries SET run_id=NULL WHERE repository_full_name=$1`,
			`DELETE FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`,
			`DELETE FROM webhook_deliveries WHERE repository_full_name=$1`,
			`DELETE FROM trigger_bindings WHERE repository_full_name=$1`,
			`DELETE FROM budgets WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`,
			`DELETE FROM repositories WHERE full_name=$1`,
		} {
			_, _ = store.Pool.Exec(ctx, statement, repo)
		}
		_, _ = store.Pool.Exec(ctx, `DELETE FROM github_installations WHERE id=$1`, installation)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM river_job WHERE kind='skip_status' AND args->>'repository'=$1`, repo)
	})
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: "http://127.0.0.1:1/v1", Model: "m", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Model: name, Prompt: "Review.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Each review is expected to cost its cap: $2.
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "pr_review", Agents: []string{name}, Enabled: true, ReviewPolicy: overload.ReviewPolicy{MaxRunCostUSD: 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO github_installations (id, account_login, account_type) VALUES ($1,'acme','Organization')`, installation); err != nil {
		t.Fatal(err)
	}
	var repoID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name, enabled, dry_run, installation_id, github_id) VALUES ($1, true, true, $2, $3) RETURNING id`, repo, installation, installation).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBinding(ctx, overload.TriggerBinding{Source: "github", Event: "pull_request", Action: "opened", Repository: repo, Workflow: name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	monthly := 5.0
	if err := store.SaveBudget(ctx, Budget{Repository: repo, MonthlyUSD: &monthly}); err != nil {
		t.Fatal(err)
	}
	// $2.50 spent this month and $0.75 earlier (last month) on this repository.
	for _, run := range []struct {
		created time.Time
		cost    float64
	}{{now.Add(-48 * time.Hour), 2.5}, {now.AddDate(0, -1, 0), 0.75}} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, created_at, cost_usd) VALUES ($1, 99, '', '', 'webhook', 'completed', $2, $3)`, repoID, run.created, run.cost); err != nil {
			t.Fatal(err)
		}
	}
	deliver := func(pr int) (string, string) {
		t.Helper()
		id := fmt.Sprintf("%s-%d", name, pr)
		delivery := PullRequestDelivery{DeliveryID: id, Action: "opened", RepoName: repo, RepoID: installation, InstallationID: installation, PR: pr, HeadSHA: strings.Repeat(fmt.Sprint(pr%10), 40), BaseSHA: strings.Repeat("a", 40), Eligible: true, Payload: []byte(fmt.Sprintf(`{"id":%q}`, id))}
		if _, err := store.IngestPR(ctx, client, delivery); err != nil {
			t.Fatal(err)
		}
		outcome, reason, _, err := store.DeliveryOutcome(ctx, "github", id)
		if err != nil {
			t.Fatal(err)
		}
		return outcome, reason
	}
	// $2.50 spent + $2 for this review fits under $5.
	if outcome, reason := deliver(1); outcome != "queued" {
		t.Fatalf("first review: %s %s", outcome, reason)
	}
	// $2.50 spent + $2 queued + $2 for this one does not.
	outcome, reason := deliver(2)
	if outcome != "skipped" || !strings.HasPrefix(reason, SkipBudgetExhausted) || !strings.Contains(reason, "monthly budget $5.00") {
		t.Fatalf("second review: %s %s", outcome, reason)
	}
	var args []byte
	if err := store.Pool.QueryRow(ctx, `SELECT args FROM river_job WHERE kind='skip_status' AND args->>'repository'=$1 ORDER BY id DESC LIMIT 1`, repo).Scan(&args); err != nil {
		t.Fatal(err)
	}
	var job SkipStatusArgs
	if err := json.Unmarshal(args, &job); err != nil || job.State != BudgetSkipSuccess || job.Description != "Overload skipped: monthly review budget reached" || job.InstallationID != installation || job.SHA != strings.Repeat("2", 40) {
		t.Fatalf("status job = %+v %v", job, err)
	}
	if err := store.SaveBudget(ctx, Budget{Repository: repo, MonthlyUSD: &monthly, SkipStatus: BudgetSkipError}); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := deliver(3); outcome != "skipped" {
		t.Fatalf("third review: %s", outcome)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT args FROM river_job WHERE kind='skip_status' AND args->>'repository'=$1 ORDER BY id DESC LIMIT 1`, repo).Scan(&args); err != nil || !strings.Contains(string(args), `"state": "error"`) && !strings.Contains(string(args), `"state":"error"`) {
		t.Fatalf("error status job = %s %v", args, err)
	}
	if err := store.SaveBudget(ctx, Budget{Repository: repo, MonthlyUSD: nil}); err != nil {
		t.Fatal(err)
	}
	if outcome, reason := deliver(4); outcome != "queued" {
		t.Fatalf("after clearing the cap: %s %s", outcome, reason)
	}
}

func TestGlobalBudgetFromEnvironment(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	store.Now = func() time.Time { return time.Date(2098, 2, 10, 0, 0, 0, 0, time.UTC) }
	t.Setenv("OVERLOAD_BUDGET_MONTHLY_USD", "100")
	t.Setenv("OVERLOAD_BUDGET_DAILY_USD", "")
	budgets, err := store.Budgets(ctx)
	if err != nil || budgets[0].Repository != "" || budgets[0].MonthlyUSD == nil || *budgets[0].MonthlyUSD != 100 || !budgets[0].FromEnvironment {
		t.Fatalf("budgets = %+v %v", budgets, err)
	}
	status, err := store.GlobalBudgetStatus(ctx)
	if err != nil || status.MonthPercent != 0 {
		t.Fatalf("status = %+v %v", status, err)
	}
	if exceeded, _, err := store.SpendExceeded(ctx, 104, 0.05); err != nil || exceeded {
		t.Fatalf("within the margin counted as exceeded: %v %v", exceeded, err)
	}
	if exceeded, period, err := store.SpendExceeded(ctx, 106, 0.05); err != nil || !exceeded || period != "monthly" {
		t.Fatalf("past the margin: %v %q %v", exceeded, period, err)
	}
	if threshold, period := (BudgetStatus{MonthlyUSD: budgets[0].MonthlyUSD, MonthPercent: 85}).Alert(); threshold != 80 || period != "monthly" {
		t.Fatalf("alert = %d %s", threshold, period)
	}
	if err := store.SaveBudget(ctx, Budget{SkipStatus: "maybe"}); err == nil {
		t.Fatal("accepted an unknown skip status")
	}
}
