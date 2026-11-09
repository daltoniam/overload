package postgres

import (
	"context"
	"fmt"
	"math"
	"sync"
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
		created  time.Time
		spend    *overload.Spend
		recorded time.Time
	}{
		{month.Add(time.Hour), &overload.Spend{USD: 1.25}, time.Time{}},
		{month.Add(2 * 24 * time.Hour), &overload.Spend{USD: 0.75}, time.Time{}},
		{now.Add(-time.Hour), &overload.Spend{USD: 2.00}, time.Time{}},
		{now.Add(-2 * time.Hour), &overload.Spend{USD: 0, Unpriced: 3}, time.Time{}},
		{now.Add(-3 * time.Hour), nil, time.Time{}},
		{month.Add(-time.Hour), &overload.Spend{USD: 50}, time.Time{}},
		// Queued last month, but its model calls ran today: the spend is
		// this month's and today's.
		{month.Add(-time.Hour), &overload.Spend{USD: 0.50}, now.Add(-30 * time.Minute)},
	}
	for _, run := range runs {
		recorded := run.created
		if !run.recorded.IsZero() {
			recorded = run.recorded
		}
		store.Now = func() time.Time { return recorded }
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
	// Four priced runs; the run with only unpriced calls is counted as
	// unpriced and left out of the average.
	if math.Abs(spend.Month-4.50) > 1e-9 || math.Abs(spend.Today-2.50) > 1e-9 || spend.MonthRuns != 4 || math.Abs(spend.AverageRun-4.50/4) > 1e-9 || spend.Unpriced != 1 {
		t.Fatalf("spend = %+v", spend)
	}
	if err := store.SetRunCost(ctx, 1, overload.Spend{USD: math.NaN()}); err == nil {
		t.Fatal("stored NaN cost")
	}
}

func TestSpendCountsEachAdditionWhenItHappened(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	repo := fmt.Sprintf("test-cost-period-%d/api", time.Now().UnixNano())
	var repoID, runID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name) VALUES ($1) RETURNING id`, repo).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id=$1`, repoID)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE id=$1`, repoID)
	})
	january := time.Date(2098, 1, 31, 23, 0, 0, 0, time.UTC)
	february := time.Date(2098, 2, 1, 1, 0, 0, 0, time.UTC)
	if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, created_at) VALUES ($1, 1, '', '', 'webhook', 'completed', $2) RETURNING id`, repoID, january).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return january }
	if err := store.AddRunCost(ctx, runID, overload.Spend{USD: 10}); err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return february }
	if err := store.AddRunCost(ctx, runID, overload.Spend{USD: 1}); err != nil {
		t.Fatal(err)
	}
	// Setting the total adds only what was not recorded yet.
	if err := store.SetRunCost(ctx, runID, overload.Spend{USD: 11.5, Unpriced: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunCost(ctx, runID, overload.Spend{USD: 11.5, Unpriced: 1}); err != nil {
		t.Fatal(err)
	}
	spend, err := store.Spend(ctx, february)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(spend.Month-1.5) > 1e-9 || math.Abs(spend.Today-1.5) > 1e-9 || spend.MonthRuns != 1 || spend.Unpriced != 1 {
		t.Fatalf("february = %+v", spend)
	}
	if spend, err = store.Spend(ctx, january); err != nil || math.Abs(spend.Month-10) > 1e-9 {
		t.Fatalf("january = %+v %v", spend, err)
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil || run.Cost == nil || math.Abs(run.Cost.USD-11.5) > 1e-9 || run.Cost.Unpriced != 1 {
		t.Fatalf("run cost = %+v %v", run.Cost, err)
	}
}

func TestRunCostLedgerMatchesTotalsUnderConcurrentWrites(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	repo := fmt.Sprintf("test-cost-ledger-%d/api", time.Now().UnixNano())
	var repoID, runID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name) VALUES ($1) RETURNING id`, repo).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id=$1`, repoID)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE id=$1`, repoID)
	})
	if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status) VALUES ($1, 1, '', '', 'webhook', 'running') RETURNING id`, repoID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for index := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if index%4 == 0 {
				errs <- store.SetRunCost(ctx, runID, overload.Spend{USD: float64(index), Unpriced: int64(index)})
			} else {
				errs <- store.AddRunCost(ctx, runID, overload.Spend{USD: 0.5, Unpriced: 1})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var total, recorded float64
	var unpriced, recordedUnpriced int64
	if err := store.Pool.QueryRow(ctx, `SELECT r.cost_usd, r.cost_unpriced_calls, COALESCE(sum(c.usd), 0), COALESCE(sum(c.unpriced_calls), 0) FROM runs r LEFT JOIN run_costs c ON c.run_id = r.id WHERE r.id=$1 GROUP BY r.id`, runID).Scan(&total, &unpriced, &recorded, &recordedUnpriced); err != nil {
		t.Fatal(err)
	}
	if math.Abs(total-recorded) > 1e-9 || unpriced != recordedUnpriced {
		t.Fatalf("run total $%v (%d unpriced) but ledger $%v (%d unpriced)", total, unpriced, recorded, recordedUnpriced)
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
	for _, filter := range []CostFilter{
		{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour), Repository: repo},
		// An unset Until means now.
		{Since: time.Now().Add(-time.Hour), Repository: repo},
	} {
		steps, err := store.TransferTimes(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range steps {
			if step.Step == "upload_seconds" && (step.Runs != 3 || step.P50 != 4 || step.Max != 40) {
				t.Fatalf("upload with until %v = %+v", filter.Until, step)
			}
			if step.Step == "extract_seconds" && step.Runs != 0 {
				t.Fatalf("extract = %+v", step)
			}
		}
	}
}
