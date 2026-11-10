package harness

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"charm.land/fantasy"
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

func TestAnthropicModelsWithoutAdaptiveThinkingGetABudget(t *testing.T) {
	recorded, err := os.ReadFile("testdata/usage/anthropic_message.json")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var thinking []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Thinking     map[string]any `json:"thinking"`
			OutputConfig map[string]any `json:"output_config"`
		}
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		thinking = append(thinking, request.Thinking)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if request.Thinking["type"] == "adaptive" || request.OutputConfig != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"adaptive thinking is not supported on this model"}}`))
			return
		}
		_, _ = w.Write(recorded)
	}))
	defer server.Close()
	model := overload.ModelProfile{Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: server.URL, Model: "claude-haiku-4-5", ReasoningParam: overload.ReasoningEffortField, ReasoningEffort: "medium"}
	if _, _, err := Complete(context.Background(), model, "Review.", "Say ok."); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// The model is remembered: the next call asks for a budget at once.
	if _, _, err := Complete(context.Background(), model, "Review.", "Say ok."); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(thinking) != 3 || thinking[0]["type"] != "adaptive" || thinking[1]["type"] != "enabled" || thinking[1]["budget_tokens"] != float64(8192) || thinking[2]["type"] != "enabled" {
		t.Fatalf("thinking requests = %v", thinking)
	}
	// An agent built before the model was remembered (a review's repair
	// call reuses it) also asks for a budget on its later calls.
	noAdaptiveThinking.Delete(server.URL + "|claude-haiku-4-5")
	agent, plan, err := newAgent(context.Background(), model, "Review.")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := plan.generate(context.Background(), agent, fantasy.AgentCall{Prompt: "Say ok."}, nil); err != nil {
			t.Fatalf("call on an older agent: %v", err)
		}
	}
	if len(thinking) != 6 || thinking[3]["type"] != "adaptive" || thinking[4]["type"] != "enabled" || thinking[5]["type"] != "enabled" {
		t.Fatalf("thinking requests on one agent = %v", thinking[3:])
	}
	if got := thinkingBudget("high", 4096); got != 2048 {
		t.Fatalf("budget within a small output limit = %d", got)
	}
	if got := thinkingBudget("high", 1500); got != 0 {
		t.Fatalf("budget too small to think = %d", got)
	}
}

// clearAnthropicEnvironment removes every credential source the Anthropic
// SDK looks for, as on a server or in a sandbox that has none.
func clearAnthropicEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// fakeMessages answers like the Messages API and records request headers.
func fakeMessages(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	recorded, err := os.ReadFile("testdata/usage/anthropic_message.json")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var last http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		last = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(recorded)
	}))
	t.Cleanup(server.Close)
	return server, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestAnthropicConnectionWithoutKeyAndAmbientCredentials(t *testing.T) {
	// A sandbox reaches the model through the model proxy with no key.
	clearAnthropicEnvironment(t)
	server, last := fakeMessages(t)
	model := overload.ModelProfile{Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: server.URL, Model: "claude-opus-5-5"}
	if _, _, err := Complete(context.Background(), model, "Review.", "Say ok."); err != nil {
		t.Fatalf("keyless connection: %v", err)
	}
	if seen := last(); seen.Get("X-Api-Key") != "" || seen.Get("Authorization") != "" {
		t.Fatalf("credentials sent without a key: %v", seen)
	}
	// Credentials in overload's own environment never reach the URL a
	// model connection names.
	clearAnthropicEnvironment(t)
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Ambient: secret\nAnthropic-Version: broken")
	if _, _, err := Complete(context.Background(), model, "Review.", "Say ok."); err != nil {
		t.Fatal(err)
	}
	if seen := last(); seen.Get("X-Api-Key") != "" || seen.Get("Authorization") != "" || seen.Get("X-Ambient") != "" || seen.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("ambient Anthropic settings were sent: %v", seen)
	}
	// The connection's own key and headers are sent.
	t.Setenv("OVERLOAD_MODEL_CLAUDE", "claude-key")
	model.APIKeyEnv, model.Headers = "OVERLOAD_MODEL_CLAUDE", map[string]string{"cf-aig-metadata": "connection"}
	if _, _, err := Complete(context.Background(), model, "Review.", "Say ok."); err != nil {
		t.Fatal(err)
	}
	if seen := last(); seen.Get("X-Api-Key") != "claude-key" || seen.Get("Cf-Aig-Metadata") != "connection" || seen.Get("Authorization") != "" {
		t.Fatalf("connection credentials = %v", seen)
	}
}

func TestAnthropicProviderDoesNotFollowRedirects(t *testing.T) {
	clearAnthropicEnvironment(t)
	t.Setenv("OVERLOAD_MODEL_CLAUDE", "claude-key")
	elsewhere, last := fakeMessages(t)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	model := overload.ModelProfile{Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: redirect.URL, Model: "claude-opus-5-5", APIKeyEnv: "OVERLOAD_MODEL_CLAUDE"}
	if _, _, err := Complete(context.Background(), model, "Review.", "Say ok."); err == nil {
		t.Fatal("a redirected call succeeded")
	}
	if seen := last(); seen != nil {
		t.Fatalf("the key followed a redirect: %v", seen)
	}
}
