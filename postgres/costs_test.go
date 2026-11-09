package postgres

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/daltoniam/overload"
)

func TestRunCostsAndSpend(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	repo := fmt.Sprintf("test-cost-%d/api", time.Now().UnixNano())
	var repoID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name) VALUES ($1) RETURNING id`, repo).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id=$1`, repoID)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE id=$1`, repoID)
	})
	// A month far in the future holds only this test's runs.
	month := time.Date(2099, 3, 1, 0, 0, 0, 0, time.UTC)
	now := month.Add(9*24*time.Hour + 12*time.Hour)
	runs := []struct {
		created time.Time
		spend   *overload.Spend
	}{
		{month.Add(time.Hour), &overload.Spend{USD: 1.25}},
		{month.Add(2 * 24 * time.Hour), &overload.Spend{USD: 0.75}},
		{now.Add(-time.Hour), &overload.Spend{USD: 2.00}},
		{now.Add(-2 * time.Hour), &overload.Spend{USD: 0, Unpriced: 3}},
		{now.Add(-3 * time.Hour), nil},
		{month.Add(-time.Hour), &overload.Spend{USD: 50}},
	}
	for _, run := range runs {
		var id int64
		if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, created_at) VALUES ($1, 1, '', '', 'webhook', 'completed', $2) RETURNING id`, repoID, run.created).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if run.spend != nil {
			if err := store.SetRunCost(ctx, id, *run.spend); err != nil {
				t.Fatal(err)
			}
		}
	}
	report, err := store.RunCosts(ctx, CostFilter{Since: month, Until: now, Repository: repo})
	if err != nil {
		t.Fatal(err)
	}
	if report.Runs != 4 || math.Abs(report.TotalUSD-4.00) > 1e-9 || math.Abs(report.AverageUSD-1.00) > 1e-9 || report.P90USD != 2.00 || report.P50USD != 0.75 || report.UnpricedRuns != 1 || report.UnpricedCalls != 3 {
		t.Fatalf("report = %+v", report)
	}
	other, err := store.RunCosts(ctx, CostFilter{Since: month, Until: now, Repository: "someone/else"})
	if err != nil || other.Runs != 0 {
		t.Fatalf("filter by repository: %+v %v", other, err)
	}
	spend, err := store.Spend(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(spend.Month-4.00) > 1e-9 || math.Abs(spend.Today-2.00) > 1e-9 || spend.MonthRuns != 4 || spend.Unpriced != 1 {
		t.Fatalf("spend = %+v", spend)
	}
	if err := store.SetRunCost(ctx, 1, overload.Spend{USD: math.NaN()}); err == nil {
		t.Fatal("stored NaN cost")
	}
}

func TestModelPricesArePinned(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	name := fmt.Sprintf("priced-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	input, cached, output := 2.0, 0.10, 10.0
	setting := overload.ReviewSettings{Name: name, Provider: "openaicompat", ConnectionKind: "hosted", BaseURL: "https://api.example.com/v1", Model: "sol", APIKeyEnv: "OVERLOAD_MODEL_KEY", PromptProfile: "context", PriceInput: &input, PriceCachedInput: &cached, PriceOutput: &output, LongContextTokens: 272000, LongContextInputMultiplier: 2, LongContextOutputMultiplier: 1.5}
	if err := store.SaveReviewSettings(ctx, setting); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Model: name, Prompt: "Review.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "pr_review", Agents: []string{name}, Enabled: true, ReviewPolicy: overload.ReviewPolicy{MaxRunCostUSD: 4}}); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.ResolveWorkflow(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	want := overload.Pricing{Input: 2, CachedInput: 0.10, CacheWrite: 2, Output: 10, LongContextTokens: 272000, LongContextInput: 2, LongContextOutput: 1.5}
	if resolved.Agents[0].Model.Pricing == nil || *resolved.Agents[0].Model.Pricing != want || resolved.MaxRunCostUSD != 4 || resolved.Verify() != nil {
		t.Fatalf("pinned = %+v %+v %v", resolved.Agents[0].Model.Pricing, resolved.ReviewPolicy, resolved.Verify())
	}
	saved, err := store.GetReviewSettings(ctx, name)
	if err != nil || saved.PriceInput == nil || *saved.PriceInput != 2 || saved.PriceCacheWrite != nil {
		t.Fatalf("saved = %+v %v", saved, err)
	}
	setting.PriceInput, setting.PriceOutput, setting.PriceCachedInput = nil, nil, nil
	setting.LongContextTokens, setting.LongContextInputMultiplier, setting.LongContextOutputMultiplier = 0, 0, 0
	if err := store.SaveReviewSettings(ctx, setting); err != nil {
		t.Fatal(err)
	}
	if again, err := store.ResolveWorkflow(ctx, name); err != nil || again.Agents[0].Model.Pricing != nil {
		t.Fatalf("unpriced model still priced: %+v %v", again.Agents[0].Model.Pricing, err)
	}
}

func TestTransferTimes(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	repo := fmt.Sprintf("test-transfer-%d/api", time.Now().UnixNano())
	var repoID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name) VALUES ($1) RETURNING id`, repo).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id=$1`, repoID)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE id=$1`, repoID)
	})
	for _, upload := range []float64{2, 4, 40} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, metrics) VALUES ($1, 1, '', '', 'webhook', 'completed', $2)`, repoID, fmt.Sprintf(`{"transfer":{"upload_seconds":%v,"archive_bytes":346000000}}`, upload)); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := store.TransferTimes(ctx, CostFilter{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour), Repository: repo})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.Step == "upload_seconds" && (step.Runs != 3 || step.P50 != 4 || step.Max != 40) {
			t.Fatalf("upload = %+v", step)
		}
		if step.Step == "extract_seconds" && step.Runs != 0 {
			t.Fatalf("extract = %+v", step)
		}
	}
}
