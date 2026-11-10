package harness

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/daltoniam/overload"
)

func TestAnthropicReviewUsesAdaptiveEffortAndPricesCache(t *testing.T) {
	recorded, err := os.ReadFile("testdata/usage/anthropic_message.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVERLOAD_MODEL_ANTHROPIC", "test-key")
	var mu sync.Mutex
	var requests []map[string]any
	var keys, paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		requests, keys, paths = append(requests, request), append(keys, r.Header.Get("X-Api-Key")), append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(recorded)
	}))
	defer server.Close()
	spec := workflowSpec(server.URL, 2, 1)
	spec.Workflow.Agents[0].Model = overload.ModelProfile{Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: server.URL, Model: "claude-sonnet-5-5", APIKeyEnv: "OVERLOAD_MODEL_ANTHROPIC", ReasoningParam: overload.ReasoningEffortField, ReasoningEffort: "high", Concurrency: 1, Pricing: &overload.Pricing{Input: 3, CachedInput: 0.30, CacheWrite: 3.75, Output: 15}}
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || paths[0] != "/v1/messages" || keys[0] != "test-key" {
		t.Fatalf("requests=%d paths=%v keys=%v", len(requests), paths, keys)
	}
	request := requests[0]
	thinking, _ := request["thinking"].(map[string]any)
	output, _ := request["output_config"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["budget_tokens"] != nil || output["effort"] != "high" || request["model"] != "claude-sonnet-5-5" {
		t.Fatalf("request = %+v", request)
	}
	// Per call: 2,000 input x $3 + 10,000 cache reads x $0.30 + 3,000 cache writes x $3.75 + 800 output x $15.
	want := 2 * (6000 + 3000 + 11250 + 12000) / 1e6
	if cost := result.Metrics["cost_usd"].(float64); math.Abs(cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
	if result.Metrics["input_tokens"] != int64(30000) || result.Metrics["cached_input_tokens"] != int64(20000) || result.Metrics["cache_write_tokens"] != int64(6000) {
		t.Fatalf("tokens = %+v", result.Metrics)
	}
}
