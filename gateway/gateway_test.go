package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
)

type fakeStore struct {
	mu       sync.Mutex
	settings []overload.ReviewSettings
	policy   catalog.Policy
	usage    []overload.Usage
}

func (store *fakeStore) AuthenticateAPIKey(_ context.Context, secret string) (overload.APIKey, *overload.User, error) {
	switch secret {
	case "ovl_good":
		return overload.APIKey{ID: 1, Prefix: "ovl_good", Scopes: []string{overload.ScopeModels}}, &overload.User{ID: 7}, nil
	case "ovl_reviews":
		return overload.APIKey{ID: 2, Prefix: "ovl_reviews", Scopes: []string{overload.ScopeReviews}}, nil, nil
	}
	return overload.APIKey{}, nil, errors.New("invalid")
}
func (store *fakeStore) ListReviewSettings(context.Context) ([]overload.ReviewSettings, error) {
	return store.settings, nil
}
func (store *fakeStore) LoadModelPolicy(context.Context) (catalog.Policy, error) {
	return store.policy, nil
}
func (store *fakeStore) RecordUsage(_ context.Context, usage overload.Usage) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.usage = append(store.usage, usage)
	return nil
}

func TestGateway(t *testing.T) {
	t.Setenv("OVERLOAD_MODEL_HOSTED", "upstream-key")
	t.Setenv("OVERLOAD_MODEL_CLAUDE", "claude-key")
	var mu sync.Mutex
	var seen []*http.Request
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		seen, bodies = append(seen, r), append(bodies, body)
		mu.Unlock()
		switch {
		case r.URL.Path == "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":30,\"output_tokens\":1}}}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":12}}\n\n")
		case body["stream"] == true:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Set-Cookie", "upstream=1")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`)
		}
	}))
	defer upstream.Close()
	store := &fakeStore{settings: []overload.ReviewSettings{
		{Name: "flint", Provider: overload.ProviderOpenAICompat, ConnectionKind: "local", BaseURL: upstream.URL + "/v1", Model: "flint", Concurrency: 1, Catalog: "recursive-flint"},
		{Name: "gpt", Provider: overload.ProviderOpenAICompat, ConnectionKind: "hosted", BaseURL: upstream.URL + "/v1/", Model: "gpt-6.1-sol", APIKeyEnv: "OVERLOAD_MODEL_HOSTED", Concurrency: 2},
		{Name: "claude", Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: upstream.URL, Model: "claude-opus-5-5", APIKeyEnv: "OVERLOAD_MODEL_CLAUDE", Concurrency: 1},
		{Name: "qwen", Provider: overload.ProviderOpenAICompat, ConnectionKind: "local", BaseURL: upstream.URL + "/v1", Model: "qwen3.8-flash-next", Concurrency: 1},
	}, policy: catalog.Policy{BlockedOrigins: []string{"CN"}}}
	gateway := New(store)
	call := func(method, path, key, body string, headers ...string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		for index := 0; index+1 < len(headers); index += 2 {
			request.Header.Set(headers[index], headers[index+1])
		}
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, request)
		return response
	}

	for name, test := range map[string]struct {
		key, path, body string
		status          int
	}{
		"no key":        {"", "/v1/models", "", http.StatusUnauthorized},
		"bad key":       {"ovl_bad", "/v1/models", "", http.StatusUnauthorized},
		"wrong scope":   {"ovl_reviews", "/v1/models", "", http.StatusUnauthorized},
		"unknown model": {"ovl_good", "/v1/chat/completions", `{"model":"nope"}`, http.StatusNotFound},
		"refused model": {"ovl_good", "/v1/chat/completions", `{"model":"qwen"}`, http.StatusNotFound},
		"wrong API":     {"ovl_good", "/v1/messages", `{"model":"flint"}`, http.StatusBadRequest},
		"not JSON":      {"ovl_good", "/v1/chat/completions", `nope`, http.StatusBadRequest},
		"unknown path":  {"ovl_good", "/v1/embeddings", `{}`, http.StatusNotFound},
	} {
		method := http.MethodPost
		if test.path == "/v1/models" {
			method = http.MethodGet
		}
		if response := call(method, test.path, test.key, test.body); response.Code != test.status {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}

	list := call(http.MethodGet, "/v1/models", "ovl_good", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"claude"`) || strings.Contains(list.Body.String(), "qwen") {
		t.Fatalf("models: %s", list.Body.String())
	}

	response := call(http.MethodPost, "/v1/chat/completions", "ovl_good", `{"model":"gpt","messages":[]}`)
	if response.Code != http.StatusOK || response.Header().Get("Set-Cookie") != "" || !strings.Contains(response.Body.String(), `"hi"`) {
		t.Fatalf("chat: %d %v %s", response.Code, response.Header(), response.Body.String())
	}
	if seen[0].URL.Path != "/v1/chat/completions" || seen[0].Header.Get("Authorization") != "Bearer upstream-key" || bodies[0]["model"] != "gpt-6.1-sol" {
		t.Fatalf("upstream: %s %q %v", seen[0].URL.Path, seen[0].Header.Get("Authorization"), bodies[0])
	}

	response = call(http.MethodPost, "/v1/chat/completions", "ovl_good", `{"model":"flint","stream":true}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("stream: %d %s", response.Code, response.Body.String())
	}
	options, _ := bodies[1]["stream_options"].(map[string]any)
	if options["include_usage"] != true || seen[1].Header.Get("Authorization") != "" {
		t.Fatalf("stream upstream: %v %q", bodies[1], seen[1].Header.Get("Authorization"))
	}

	response = call(http.MethodPost, "/v1/messages", "", `{"model":"claude","stream":true}`, "X-Api-Key", "ovl_good", "Anthropic-Beta", "tools")
	if response.Code != http.StatusOK || seen[2].Header.Get("X-Api-Key") != "claude-key" || seen[2].Header.Get("Anthropic-Version") == "" || seen[2].Header.Get("Anthropic-Beta") != "tools" || bodies[2]["model"] != "claude-opus-5-5" {
		t.Fatalf("messages: %d %v %v", response.Code, seen[2].Header, bodies[2])
	}

	want := []string{"gpt 9/3", "flint 11/5", "claude 30/12"}
	if len(store.usage) != len(want) {
		t.Fatalf("usage = %+v", store.usage)
	}
	for index, usage := range store.usage {
		if got := fmt.Sprintf("%s %d/%d", usage.Model, usage.InputTokens, usage.OutputTokens); got != want[index] || usage.UserID != 7 || usage.KeyID != 1 || usage.Status != http.StatusOK {
			t.Errorf("usage %d = %+v", index, usage)
		}
	}
}

func TestGatewayQueueFull(t *testing.T) {
	store := &fakeStore{settings: []overload.ReviewSettings{{Name: "flint", Provider: overload.ProviderOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", Model: "flint", Concurrency: 1}}}
	gateway := New(store)
	queue := gateway.queue(store.settings[0])
	queue.maxWaiting = 0
	release, _ := queue.acquire(context.Background(), "other")
	defer release()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"flint"}`))
	request.Header.Set("Authorization", "Bearer ovl_good")
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("queue full: %d", response.Code)
	}
}

func TestTokenCount(t *testing.T) {
	var count tokenCount
	scanner := &lineScanner{observe: count.observe, sse: true}
	scanner.write([]byte("data: {\"usage\":{\"prompt_"))
	scanner.write([]byte("tokens\":4,\"completion_tokens\":2}}\n"))
	scanner.write([]byte("data: {\"usage\":{\"completion_tokens\":6}}"))
	scanner.finish()
	if count.input != 4 || count.output != 6 {
		t.Fatalf("count = %+v", count)
	}
}
