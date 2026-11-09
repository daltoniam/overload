package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	anthropicconfig "github.com/anthropics/anthropic-sdk-go/config"
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
	path, key, authorization, version, beta, ambient, metadata, workspace, telemetry string
	body                                                                             map[string]any
}

// fakeAnthropic answers every request with anthropicStream and records the
// last request.
func fakeAnthropic(t *testing.T) (*httptest.Server, func() anthropicRequest) {
	t.Helper()
	var mu sync.Mutex
	var last anthropicRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen := anthropicRequest{path: r.URL.Path, key: r.Header.Get("X-Api-Key"), authorization: r.Header.Get("Authorization"), version: r.Header.Get("Anthropic-Version"), beta: r.Header.Get("Anthropic-Beta"), ambient: r.Header.Get("X-Ambient"), metadata: r.Header.Get("Cf-Aig-Metadata"), workspace: r.Header.Get("Anthropic-Workspace-Id"), telemetry: r.Header.Get("X-Stainless-Lang")}
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
	if text != "looks fine" || seen.path != "/v1/messages" || seen.key != "claude-key" || seen.authorization != "" || seen.version != "2023-06-01" || seen.telemetry != "" {
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
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Ambient: secret\ncf-aig-metadata: ambient\nAnthropic-Version: broken")
	model.Headers = map[string]string{"cf-aig-metadata": "connection"}
	if _, err := Complete(context.Background(), model, "You review code.", "Review this."); err != nil {
		t.Fatal(err)
	}
	if seen := last(); seen.ambient != "" || seen.metadata != "connection" || seen.version != "2023-06-01" {
		t.Fatalf("ANTHROPIC_CUSTOM_HEADERS were sent: %+v", seen)
	}

	// A profile's non-credential settings apply whatever key is given.
	clearAnthropicEnvironment(t)
	config := t.TempDir()
	if err := os.MkdirAll(filepath.Join(config, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "configs", "default.json"), []byte(`{"version":"1","workspace_id":"wrkspc_secret","base_url":"https://elsewhere.invalid","authentication":{"type":"user_oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_CONFIG_DIR", config)
	if profile, err := anthropicconfig.LoadProfile(config, "default"); err != nil || profile.WorkspaceID != "wrkspc_secret" {
		t.Fatalf("the test profile does not load: %+v, %v", profile, err)
	}
	model.Headers = nil
	if _, err := Complete(context.Background(), model, "You review code.", "Review this."); err != nil {
		t.Fatal(err)
	}
	if seen := last(); seen.workspace != "" || seen.key != "" || seen.authorization != "" {
		t.Fatalf("the host's Anthropic profile was sent: %+v", seen)
	}
}

// TestAnthropicAgentUsesToolsWhileStreaming runs a tool-using agent over two
// streamed turns: thinking and a tool call, then the answer.
func TestAnthropicAgentUsesToolsWhileStreaming(t *testing.T) {
	clearAnthropicEnvironment(t)
	t.Setenv("OVERLOAD_TOOL_TEST", "sb_test")
	fixture := newMCPFixture(t, "unused")
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		turn := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if turn > 1 {
			_, _ = io.WriteString(w, anthropicStream)
			return
		}
		_, _ = io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_0","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Search first."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"sb_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"issues\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	defer server.Close()
	toolset, err := ConnectTools(context.Background(), []overload.ToolServer{fixture.toolServer()}, "", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = toolset.Close() }()
	model := overload.ModelProfile{Name: "claude", Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: server.URL, Model: "claude-opus-5-5", ReasoningParam: overload.ReasoningEffortField, ReasoningEffort: "high"}
	result, err := RunAgent(context.Background(), model, "Research.", "go", toolset, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Steps != 2 || result.ToolCalls != 1 || result.HitStepLimit || result.Text != "looks fine" {
		t.Fatalf("result = %+v", result)
	}
	fixture.mu.Lock()
	calls := fmt.Sprint(fixture.calls)
	fixture.mu.Unlock()
	if calls != "[search:issues]" {
		t.Fatalf("tool calls = %s", calls)
	}
	second := fmt.Sprint(bodies[1]["messages"])
	for _, want := range []string{"sig-1", "Search first.", "tool_result", "toolu_1"} {
		if !strings.Contains(second, want) {
			t.Fatalf("second turn lacks %q: %s", want, second)
		}
	}
	if bodies[1]["stream"] != true {
		t.Fatalf("second turn did not stream: %v", bodies[1])
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
