package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/modelproxy"
)

// anthropicStream is a minimal streamed Messages API answer.
const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"looks fine"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicRequest is what a fake Messages API saw.
type anthropicRequest struct {
	path, key, authorization, version, beta, ambient, metadata string
	body                                                       map[string]any
}

// fakeAnthropic answers every request with anthropicStream and records the
// last request.
func fakeAnthropic(t *testing.T) (*httptest.Server, func() anthropicRequest) {
	t.Helper()
	var mu sync.Mutex
	var last anthropicRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen := anthropicRequest{path: r.URL.Path, key: r.Header.Get("X-Api-Key"), authorization: r.Header.Get("Authorization"), version: r.Header.Get("Anthropic-Version"), beta: r.Header.Get("Anthropic-Beta"), ambient: r.Header.Get("X-Ambient"), metadata: r.Header.Get("Cf-Aig-Metadata")}
		_ = json.Unmarshal(raw, &seen.body)
		mu.Lock()
		last = seen
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicStream)
	}))
	t.Cleanup(server.Close)
	return server, func() anthropicRequest {
		mu.Lock()
		defer mu.Unlock()
		return last
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

func TestAnthropicProviderThinksWithEffort(t *testing.T) {
	clearAnthropicEnvironment(t)
	t.Setenv("OVERLOAD_MODEL_CLAUDE", "claude-key")
	server, last := fakeAnthropic(t)
	model := overload.ModelProfile{Name: "claude", Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: server.URL, Model: "claude-opus-5-5", APIKeyEnv: "OVERLOAD_MODEL_CLAUDE", ReasoningParam: overload.ReasoningEffortField, ReasoningEffort: "high"}
	text, err := Complete(context.Background(), model, "You review code.", "Review this.")
	if err != nil {
		t.Fatal(err)
	}
	seen := last()
	if text != "looks fine" || seen.path != "/v1/messages" || seen.key != "claude-key" || seen.authorization != "" || seen.version == "" {
		t.Fatalf("text=%q request=%+v", text, seen)
	}
	config, _ := seen.body["output_config"].(map[string]any)
	thinking, _ := seen.body["thinking"].(map[string]any)
	if config["effort"] != "high" || thinking["type"] != "adaptive" || seen.body["model"] != "claude-opus-5-5" || seen.body["max_tokens"] != float64(hostedMaxOutputTokens) || seen.body["stream"] != true {
		t.Fatalf("request body = %v", seen.body)
	}
}

func TestAnthropicProviderWithoutKey(t *testing.T) {
	clearAnthropicEnvironment(t)
	server, last := fakeAnthropic(t)
	// A local Anthropic-compatible server with the local output limit, which
	// the SDK refuses to send without streaming.
	model := overload.ModelProfile{Name: "local", Provider: overload.ProviderAnthropic, ConnectionKind: "local", BaseURL: server.URL, Model: "claude-opus-5-5"}
	if _, err := Complete(context.Background(), model, "You review code.", "Review this."); err != nil {
		t.Fatal(err)
	}
	seen := last()
	if seen.key != "" || seen.authorization != "" {
		t.Fatalf("credentials sent without a key: %+v", seen)
	}
	if _, ok := seen.body["output_config"]; ok || seen.body["max_tokens"] != float64(localMaxOutputTokens) {
		t.Fatalf("request body = %v", seen.body)
	}
}

func TestAnthropicProviderIgnoresAmbientCredentials(t *testing.T) {
	clearAnthropicEnvironment(t)
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token")
	server, last := fakeAnthropic(t)
	model := overload.ModelProfile{Name: "claude", Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: server.URL, Model: "claude-opus-5-5"}
	if _, err := Complete(context.Background(), model, "You review code.", "Review this."); err != nil {
		t.Fatal(err)
	}
	if seen := last(); seen.key != "" || seen.authorization != "" {
		t.Fatalf("ambient Anthropic credentials were sent: %+v", seen)
	}

	clearAnthropicEnvironment(t)
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Ambient: secret\ncf-aig-metadata: ambient")
	model.Headers = map[string]string{"cf-aig-metadata": "connection"}
	if _, err := Complete(context.Background(), model, "You review code.", "Review this."); err != nil {
		t.Fatal(err)
	}
	if seen := last(); seen.ambient != "" || seen.metadata != "connection" {
		t.Fatalf("ANTHROPIC_CUSTOM_HEADERS were sent: %+v", seen)
	}
}

func TestAnthropicProviderDoesNotFollowRedirects(t *testing.T) {
	clearAnthropicEnvironment(t)
	t.Setenv("OVERLOAD_MODEL_CLAUDE", "claude-key")
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	model := overload.ModelProfile{Name: "claude", Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: server.URL, Model: "claude-opus-5-5", APIKeyEnv: "OVERLOAD_MODEL_CLAUDE"}
	if _, err := Complete(context.Background(), model, "You review code.", "Review this."); err == nil {
		t.Fatal("a redirect was treated as an answer")
	}
	if count := elsewhere.Load(); count != 0 {
		t.Fatalf("the key followed a redirect %d times", count)
	}
}

// TestAnthropicThroughModelProxy runs the connection a sandboxed agent gets:
// no key and the model proxy's base URL. The proxy adds the key.
func TestAnthropicThroughModelProxy(t *testing.T) {
	clearAnthropicEnvironment(t)
	t.Setenv("OVERLOAD_MODEL_CLAUDE", "claude-key")
	upstream, last := fakeAnthropic(t)
	proxy := modelproxy.New("http://overload.invalid")
	server := httptest.NewServer(proxy)
	defer server.Close()
	model := overload.ModelProfile{Name: "claude", Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: upstream.URL, Model: "claude-opus-5-5", APIKeyEnv: "OVERLOAD_MODEL_CLAUDE", ReasoningParam: overload.ReasoningEffortField, ReasoningEffort: "high"}
	base, revoke := proxy.Grant(model, time.Minute)
	defer revoke()
	granted := model
	granted.BaseURL, granted.APIKeyEnv = server.URL+strings.TrimPrefix(base, "http://overload.invalid"), ""
	text, err := Complete(context.Background(), granted, "You review code.", "Review this.")
	if err != nil {
		t.Fatal(err)
	}
	seen := last()
	if text != "looks fine" || seen.path != "/v1/messages" || seen.key != "claude-key" || seen.beta != "" || seen.body["stream"] != true {
		t.Fatalf("text=%q request=%+v", text, seen)
	}
}

func TestStreamCallCopiesEveryField(t *testing.T) {
	callType, streamType := reflect.TypeFor[fantasy.AgentCall](), reflect.TypeFor[fantasy.AgentStreamCall]()
	for field := range callType.Fields() {
		match, ok := streamType.FieldByName(field.Name)
		if !ok || match.Type != field.Type {
			t.Errorf("AgentStreamCall has no field %s %s; update streamCall", field.Name, field.Type)
		}
	}
	limit := int64(7)
	call := fantasy.AgentCall{Prompt: "p", MaxOutputTokens: &limit, StopWhen: []fantasy.StopCondition{fantasy.StepCountIs(3)}, Headers: map[string]string{"a": "b"}}
	stream := streamCall(call)
	if stream.Prompt != "p" || stream.MaxOutputTokens != &limit || len(stream.StopWhen) != 1 || stream.Headers["a"] != "b" {
		t.Fatalf("stream call = %+v", stream)
	}
}

func TestAnthropicPlan(t *testing.T) {
	plan := planFor(overload.ModelProfile{Provider: overload.ProviderAnthropic, ConnectionKind: "local"})
	if plan.param != overload.ReasoningAuto || plan.providerOptions("high") != nil {
		t.Fatalf("a local Anthropic-style model got the chat template switch: %+v", plan)
	}
	plan = planFor(overload.ModelProfile{Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", ReasoningParam: overload.ReasoningEffortField, ReasoningEffort: "xhigh"})
	if !plan.canFallBack() || plan.providerOptions("medium") == nil || plan.label() != "reasoning_effort:xhigh" {
		t.Fatalf("plan = %+v", plan)
	}
}
