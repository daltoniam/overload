package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daltoniam/overload"
)

func TestAnthropicProviderThinksWithEffort(t *testing.T) {
	t.Setenv("OVERLOAD_MODEL_CLAUDE", "claude-key")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token")
	var body map[string]any
	var path, key, authorization, version string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key, authorization, version = r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), r.Header.Get("Anthropic-Version")
		raw, _ := io.ReadAll(r.Body)
		body = nil
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"looks fine"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`)
	}))
	defer server.Close()
	model := overload.ModelProfile{Name: "claude", Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: server.URL, Model: "claude-opus-5-5", APIKeyEnv: "OVERLOAD_MODEL_CLAUDE", ReasoningParam: overload.ReasoningEffortField, ReasoningEffort: "high"}
	text, err := Complete(context.Background(), model, "You review code.", "Review this.")
	if err != nil {
		t.Fatal(err)
	}
	if text != "looks fine" || path != "/v1/messages" || key != "claude-key" || authorization != "" || version == "" {
		t.Fatalf("text=%q path=%q key=%q authorization=%q version=%q", text, path, key, authorization, version)
	}
	config, _ := body["output_config"].(map[string]any)
	thinking, _ := body["thinking"].(map[string]any)
	if config["effort"] != "high" || thinking["type"] != "adaptive" || body["model"] != "claude-opus-5-5" || body["max_tokens"] != float64(hostedMaxOutputTokens) {
		t.Fatalf("request body = %v", body)
	}

	model.APIKeyEnv, model.ReasoningParam, model.ReasoningEffort = "", overload.ReasoningNone, ""
	if _, err := Complete(context.Background(), model, "You review code.", "Review this."); err != nil {
		t.Fatal(err)
	}
	if key != "" || authorization != "" {
		t.Fatalf("ambient Anthropic credentials were sent: key=%q authorization=%q", key, authorization)
	}
	if _, ok := body["output_config"]; ok {
		t.Fatalf("effort sent without a reasoning setting: %v", body)
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
