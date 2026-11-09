package harness

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/daltoniam/overload"
)

// recordedServer answers every chat completion with a recorded response
// whose usage is 12,000 prompt tokens (10,000 cached) and 800 output.
func recordedServer(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	recorded, err := os.ReadFile("testdata/usage/chat_completion.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(recorded)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestReviewCostMatchesHandCalculation(t *testing.T) {
	sol := &overload.Pricing{Input: 2, CachedInput: 0.10, CacheWrite: 2, Output: 10}
	longContext := &overload.Pricing{Input: 2, CachedInput: 0.10, CacheWrite: 2, Output: 10, LongContextTokens: 11000, LongContextInput: 2, LongContextOutput: 1.5}
	for _, test := range []struct {
		name     string
		pricing  *overload.Pricing
		wantUSD  float64
		unpriced int64
	}{
		// Per call: 2,000 uncached x $2 + 10,000 cached x $0.10 + 800 output x $10 per million.
		{"priced", sol, 3 * 0.013, 0},
		// Above 11,000 prompt tokens: input x2 and output x1.5.
		{"long context", longContext, 3 * 0.022, 0},
		{"unpriced", nil, 0, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			server := recordedServer(t, &calls)
			spec := workflowSpec(server.URL, 3, 1)
			spec.Workflow.Agents[0].Model.Pricing = test.pricing
			result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
			if err != nil {
				t.Fatal(err)
			}
			cost := result.Metrics["cost_usd"].(float64)
			if math.Round(cost*100) != math.Round(test.wantUSD*100) || math.Abs(cost-test.wantUSD) > 1e-9 {
				t.Fatalf("cost = %v, want %v", cost, test.wantUSD)
			}
			if result.Metrics["cost_unpriced_calls"] != test.unpriced || result.Metrics["model_calls"] != int64(3) {
				t.Fatalf("metrics = %+v", result.Metrics)
			}
			if result.Metrics["input_tokens"] != int64(36000) || result.Metrics["cached_input_tokens"] != int64(30000) || result.Metrics["output_tokens"] != int64(2400) {
				t.Fatalf("token metrics = %+v", result.Metrics)
			}
			agent := result.Metrics["routing"].(overload.Routing).Agents[0]
			if math.Abs(agent.CostUSD-test.wantUSD) > 1e-9 || agent.Calls != 3 || agent.UnpricedCalls != test.unpriced {
				t.Fatalf("agent = %+v", agent)
			}
			label := agent.Spend().Label(agent.Calls)
			if test.pricing == nil && label != "unpriced" {
				t.Fatalf("unpriced label = %q", label)
			}
			if test.pricing != nil && strings.Contains(label, "unpriced") {
				t.Fatalf("priced label = %q", label)
			}
		})
	}
}

func TestRunCostCapCompletesDegradedWithinCap(t *testing.T) {
	var calls atomic.Int64
	server := recordedServer(t, &calls)
	spec := workflowSpec(server.URL, 6, 1)
	spec.Workflow.Version = overload.SnapshotVersion
	spec.Workflow.Agents[0].Model.Pricing = &overload.Pricing{Input: 2, CachedInput: 0.10, CacheWrite: 2, Output: 10}
	spec.Workflow.Agents[0].Model.MaxOutputTokens = 1000
	// Each call reserves its prompt as uncached input plus 1,000 output
	// tokens (about $0.0101) and costs $0.013; three fit under $0.04.
	spec.Workflow.MaxRunCostUSD = 0.04
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	cost := result.Metrics["cost_usd"].(float64)
	routing := result.Metrics["routing"].(overload.Routing)
	if cost > spec.Workflow.MaxRunCostUSD || calls.Load() == 0 || calls.Load() == 6 {
		t.Fatalf("cost = %v with %d calls", cost, calls.Load())
	}
	if result.Metrics["degraded"] != true || !strings.Contains(strings.Join(routing.Degraded, "\n"), "cost cap reached") {
		t.Fatalf("not degraded by the cap: %+v", routing.Degraded)
	}
	agent := routing.Agents[0]
	if agent.CostCapped != 6-int(calls.Load()) || len(agent.Unreviewed) != agent.CostCapped || agent.Failed != "" {
		t.Fatalf("agent = %+v", agent)
	}
}

func TestPricingCost(t *testing.T) {
	pricing := &overload.Pricing{Input: 3, CachedInput: 0.30, CacheWrite: 3.75, Output: 15}
	// 1,000 uncached x $3 + 2,000 cache reads x $0.30 + 4,000 cache writes x $3.75 + 500 output x $15.
	usd, ok := pricing.Cost(overload.Usage{Input: 7000, CachedInput: 2000, CacheWrite: 4000, Output: 500})
	if !ok || math.Abs(usd-(3000+600+15000+7500)/1e6) > 1e-12 {
		t.Fatalf("cost = %v %v", usd, ok)
	}
	if _, ok := (*overload.Pricing)(nil).Cost(overload.Usage{Input: 1}); ok {
		t.Fatal("nil pricing priced a call")
	}
}
