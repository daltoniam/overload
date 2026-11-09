package modelproxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
)

func TestParseUsage(t *testing.T) {
	for _, test := range []struct {
		name, provider, contentType, body string
		want                              overload.Usage
	}{
		{"chat completions", overload.ProviderOpenAICompat, "application/json", `{"usage":{"prompt_tokens":12000,"completion_tokens":800,"prompt_tokens_details":{"cached_tokens":10000}}}`, overload.Usage{Calls: 1, Input: 12000, CachedInput: 10000, Output: 800}},
		{"responses", overload.ProviderOpenAICompat, "application/json", `{"usage":{"input_tokens":5000,"output_tokens":300,"input_tokens_details":{"cached_tokens":4000}}}`, overload.Usage{Calls: 1, Input: 5000, CachedInput: 4000, Output: 300}},
		{"anthropic", overload.ProviderAnthropic, "application/json", `{"usage":{"input_tokens":2000,"output_tokens":800,"cache_read_input_tokens":10000,"cache_creation_input_tokens":3000}}`, overload.Usage{Calls: 1, Input: 15000, CachedInput: 10000, CacheWrite: 3000, Output: 800}},
		{"chat stream", overload.ProviderOpenAICompat, "text/event-stream", "data: {\"choices\":[]}\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7}}\n\ndata: [DONE]\n", overload.Usage{Calls: 1, Input: 100, Output: 7}},
		{"responses stream", overload.ProviderOpenAICompat, "text/event-stream", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":50,\"output_tokens\":5}}}\n", overload.Usage{Calls: 1, Input: 50, Output: 5}},
		{"anthropic stream", overload.ProviderAnthropic, "text/event-stream", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":100,\"output_tokens\":1}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n", overload.Usage{Calls: 1, Input: 120, CachedInput: 100, Output: 40}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseUsage(test.provider, test.contentType, []byte(test.body))
			if !ok || got != test.want {
				t.Fatalf("usage = %+v %v, want %+v", got, ok, test.want)
			}
		})
	}
	if _, ok := parseUsage(overload.ProviderOpenAICompat, "application/json", []byte(`{"error":"x"}`)); ok {
		t.Fatal("usage read from an error")
	}
}

func TestProxyMetersSpendAndStopsAtTheBudget(t *testing.T) {
	t.Setenv("OVERLOAD_MODEL_TEST", "key")
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":12000,"completion_tokens":800,"prompt_tokens_details":{"cached_tokens":10000}}}`)
	}))
	defer upstream.Close()
	proxy := New("http://overload:8083")
	var seen []float64
	stop := false
	proxy.Budget = func(_ context.Context, pending float64) (string, error) {
		seen = append(seen, pending)
		if stop {
			return "monthly", nil
		}
		return "", nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	base, revoke := proxy.Grant(overload.ModelProfile{BaseURL: upstream.URL + "/v1", Model: "sol", APIKeyEnv: "OVERLOAD_MODEL_TEST", Pricing: &overload.Pricing{Input: 2, CachedInput: 0.10, Output: 10}}, time.Minute)
	path := strings.TrimPrefix(base, "http://overload:8083")
	post := func() int {
		response, err := http.Post(server.URL+path+"/chat/completions", "application/json", strings.NewReader(`{"model":"sol"}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response.StatusCode
	}
	for range 2 {
		if status := post(); status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
	}
	// Each call: 2,000 uncached x $2 + 10,000 cached x $0.10 + 800 output x $10 per million = $0.013.
	if pending := proxy.Pending(); math.Abs(pending-0.026) > 1e-9 || len(seen) != 2 || math.Abs(seen[1]-0.013) > 1e-9 {
		t.Fatalf("pending = %v seen = %v", pending, seen)
	}
	stop = true
	if status := post(); status != http.StatusTooManyRequests || calls != 2 {
		t.Fatalf("over budget: status %d after %d upstream calls", status, calls)
	}
	revoke()
	if pending := proxy.Pending(); pending != 0 {
		t.Fatalf("revoked grant still pending: %v", pending)
	}
}

func TestUsageMeterSurvivesHugeAnswers(t *testing.T) {
	meter := newUsageMeter(overload.ProviderAnthropic, "text/event-stream")
	meter.write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n"))
	// A content line over the line limit is skipped, not fatal.
	meter.write([]byte("data: {\"delta\":\"" + strings.Repeat("x", maxEventLine+10)))
	meter.write([]byte("\"}\n\n"))
	// Answers past the size limit are still read as they stream.
	for range 20 {
		meter.write([]byte("data: {\"delta\":\"" + strings.Repeat("y", 1<<20-100) + "\"}\n\n"))
	}
	meter.write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4000}}\n"))
	usage, exact := meter.usage()
	if !exact || usage.Input != 20 || usage.Output != 4000 || meter.bytes < maxMeteredBytes {
		t.Fatalf("usage = %+v exact = %v after %d bytes", usage, exact, meter.bytes)
	}
	plain := newUsageMeter(overload.ProviderOpenAICompat, "application/json")
	plain.write([]byte(strings.Repeat("z", 3000)))
	if usage, exact := plain.usage(); exact || usage.Output != 1001 {
		t.Fatalf("estimate = %+v %v", usage, exact)
	}
}
