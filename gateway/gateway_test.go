package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
		return overload.APIKey{ID: 1, Prefix: "ovl_good", Scopes: []string{overload.ScopeModels}}, &overload.User{ID: 7, Role: overload.RoleMember}, nil
	case "ovl_reviews":
		return overload.APIKey{ID: 2, Prefix: "ovl_reviews", Scopes: []string{overload.ScopeReviews}}, nil, nil
	case "ovl_viewer":
		return overload.APIKey{ID: 3, Prefix: "ovl_viewer", Scopes: []string{overload.ScopeModels}}, &overload.User{ID: 8, Role: overload.RoleViewer}, nil
	case "ovl_shared":
		return overload.APIKey{ID: 4, Prefix: "ovl_shared", Scopes: []string{overload.ScopeModels}}, nil, nil
	case "ovl_database_down":
		return overload.APIKey{}, nil, errors.New("connection refused")
	}
	return overload.APIKey{}, nil, overload.ErrInvalidAPIKey
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
func (store *fakeStore) recorded() []overload.Usage {
	store.mu.Lock()
	defer store.mu.Unlock()
	return slices.Clone(store.usage)
}

// fakeUpstream is a model server that records what reached it. It answers
// on the server's goroutines, so reads take the lock too.
type fakeUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
	bodies  []map[string]any
}

func (upstream *fakeUpstream) request(index int) (http.Header, map[string]any) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return upstream.headers[index], upstream.bodies[index]
}

func (upstream *fakeUpstream) count() int {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return len(upstream.headers)
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	upstream := &fakeUpstream{}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the gateway followed a redirect")
	}))
	t.Cleanup(elsewhere.Close)
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		upstream.mu.Lock()
		upstream.headers, upstream.bodies = append(upstream.headers, r.Header.Clone()), append(upstream.bodies, body)
		upstream.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/denied/"):
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
		case strings.HasPrefix(r.URL.Path, "/redirect/"):
			http.Redirect(w, r, elsewhere.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
		case r.URL.Path == "/v1/messages" && body["stream"] == true:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":30,\"cache_read_input_tokens\":900,\"cache_creation_input_tokens\":70,\"output_tokens\":1}}}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":12}}\n\n")
		case r.URL.Path == "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":20,"output_tokens":7}}`)
		case body["stream"] == true:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Set-Cookie", "upstream=1")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestGateway(t *testing.T) {
	t.Setenv("OVERLOAD_MODEL_HOSTED", "upstream-key")
	t.Setenv("OVERLOAD_MODEL_CLAUDE", "claude-key")
	upstream := newFakeUpstream(t)
	store := &fakeStore{settings: []overload.ReviewSettings{
		{Name: "flint", Provider: overload.ProviderOpenAICompat, ConnectionKind: "local", BaseURL: upstream.URL + "/v1", Model: "flint", Concurrency: 1, Catalog: "recursive-flint"},
		{Name: "gpt", Provider: overload.ProviderOpenAICompat, ConnectionKind: "hosted", BaseURL: upstream.URL + "/v1/", Model: "gpt-6.1-sol", APIKeyEnv: "OVERLOAD_MODEL_HOSTED", Concurrency: 2},
		{Name: "claude", Provider: overload.ProviderAnthropic, ConnectionKind: "hosted", BaseURL: upstream.URL, Model: "claude-opus-5-5", APIKeyEnv: "OVERLOAD_MODEL_CLAUDE", Concurrency: 1},
		{Name: "qwen", Provider: overload.ProviderOpenAICompat, ConnectionKind: "local", BaseURL: upstream.URL + "/v1", Model: "qwen3.8-flash-next", Concurrency: 1},
		{Name: "denied", Provider: overload.ProviderOpenAICompat, ConnectionKind: "hosted", BaseURL: upstream.URL + "/denied/v1", Model: "denied", APIKeyEnv: "OVERLOAD_MODEL_HOSTED", Concurrency: 1},
		{Name: "moved", Provider: overload.ProviderOpenAICompat, ConnectionKind: "hosted", BaseURL: upstream.URL + "/redirect/v1", Model: "moved", APIKeyEnv: "OVERLOAD_MODEL_HOSTED", Concurrency: 1},
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
		headers         []string
		status          int
		message         string
	}{
		"no key":               {path: "/v1/models", status: http.StatusUnauthorized},
		"bad key":              {key: "ovl_bad", path: "/v1/models", status: http.StatusUnauthorized},
		"database down":        {key: "ovl_database_down", path: "/v1/models", status: http.StatusServiceUnavailable, message: "retry later"},
		"wrong scope":          {key: "ovl_reviews", path: "/v1/models", status: http.StatusForbidden},
		"viewer":               {key: "ovl_viewer", path: "/v1/models", status: http.StatusForbidden},
		"shared key":           {key: "ovl_shared", path: "/v1/models", status: http.StatusOK},
		"lowercase bearer":     {path: "/v1/models", headers: []string{"Authorization", "bearer ovl_good"}, status: http.StatusOK},
		"uppercase bearer":     {path: "/v1/models", headers: []string{"Authorization", "BEARER  ovl_good"}, status: http.StatusOK},
		"other scheme":         {path: "/v1/models", headers: []string{"Authorization", "Basic ovl_good"}, status: http.StatusUnauthorized},
		"unknown model":        {key: "ovl_good", path: "/v1/chat/completions", body: `{"model":"nope"}`, status: http.StatusNotFound},
		"refused model":        {key: "ovl_good", path: "/v1/chat/completions", body: `{"model":"qwen"}`, status: http.StatusNotFound},
		"wrong API":            {key: "ovl_good", path: "/v1/messages", body: `{"model":"flint"}`, status: http.StatusBadRequest},
		"not JSON":             {key: "ovl_good", path: "/v1/chat/completions", body: `nope`, status: http.StatusBadRequest},
		"unknown path":         {key: "ovl_good", path: "/v1/embeddings", body: `{}`, status: http.StatusNotFound},
		"upstream refuses key": {key: "ovl_good", path: "/v1/chat/completions", body: `{"model":"denied"}`, status: http.StatusBadGateway, message: "rejected overload's credentials"},
		"upstream redirects":   {key: "ovl_good", path: "/v1/chat/completions", body: `{"model":"moved"}`, status: http.StatusTemporaryRedirect},
	} {
		method := http.MethodPost
		if test.path == "/v1/models" {
			method = http.MethodGet
		}
		response := call(method, test.path, test.key, test.body, test.headers...)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.message) {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	var statuses []int
	for _, usage := range store.recorded() {
		statuses = append(statuses, usage.Status)
	}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []int{http.StatusTemporaryRedirect, http.StatusBadGateway}) || upstream.count() != 2 {
		t.Fatalf("forwarded calls: statuses %v, upstream requests %d", statuses, upstream.count())
	}
	before, beforeUsage := upstream.count(), len(store.recorded())

	list := call(http.MethodGet, "/v1/models", "ovl_good", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"claude"`) || strings.Contains(list.Body.String(), "qwen") {
		t.Fatalf("models: %s", list.Body.String())
	}

	response := call(http.MethodPost, "/v1/chat/completions", "ovl_good", `{"model":"gpt","messages":[]}`, "X-Api-Key", "ovl_good")
	if response.Code != http.StatusOK || response.Header().Get("Set-Cookie") != "" || !strings.Contains(response.Body.String(), `"hi"`) {
		t.Fatalf("chat: %d %v %s", response.Code, response.Header(), response.Body.String())
	}
	headers, body := upstream.request(before)
	if headers.Get("Authorization") != "Bearer upstream-key" || headers.Get("X-Api-Key") != "" || body["model"] != "gpt-6.1-sol" {
		t.Fatalf("upstream: %v %v", headers, body)
	}

	response = call(http.MethodPost, "/v1/chat/completions", "ovl_good", `{"model":"flint","stream":true,"stream_options":{"include_usage":true}}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "[DONE]") || !strings.Contains(response.Body.String(), `"prompt_tokens":11`) {
		t.Fatalf("stream with usage: %d %s", response.Code, response.Body.String())
	}
	headers, body = upstream.request(before + 1)
	if options, _ := body["stream_options"].(map[string]any); options["include_usage"] != true || headers.Get("Authorization") != "" {
		t.Fatalf("stream upstream: %v %q", body, headers.Get("Authorization"))
	}

	for index, request := range []string{`{"model":"flint","stream":true,"stream_options":{"include_usage":false}}`, `{"model":"flint","stream":true}`} {
		response = call(http.MethodPost, "/v1/chat/completions", "ovl_good", request)
		want := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\ndata: [DONE]\n\n"
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Fatalf("%s: %d %q", request, response.Code, response.Body.String())
		}
		if _, body := upstream.request(before + 2 + index); body["stream_options"].(map[string]any)["include_usage"] != true {
			t.Fatalf("usage not requested upstream: %v", body)
		}
	}

	response = call(http.MethodPost, "/v1/messages", "", `{"model":"claude","stream":true}`, "X-Api-Key", "ovl_good", "Anthropic-Beta", "mcp-client-2025-04-04")
	headers, body = upstream.request(before + 4)
	if response.Code != http.StatusOK || headers.Get("X-Api-Key") != "claude-key" || headers.Get("Anthropic-Version") != defaultAnthropicVersion || headers.Get("Anthropic-Beta") != "" || headers.Get("Authorization") != "" || body["model"] != "claude-opus-5-5" {
		t.Fatalf("messages: %d %v %v", response.Code, headers, body)
	}

	response = call(http.MethodPost, "/v1/messages", "ovl_good", `{"model":"claude"}`, "Anthropic-Version", "2025-01-01")
	if headers, _ := upstream.request(before + 5); response.Code != http.StatusOK || headers.Get("Anthropic-Version") != "2025-01-01" || headers.Get("Authorization") != "" {
		t.Fatalf("messages without streaming: %d %v", response.Code, headers)
	}

	want := []string{"gpt 9/3", "flint 11/5", "flint 11/5", "flint 11/5", "claude 1000/12", "claude 20/7"}
	usage := store.recorded()[beforeUsage:]
	if len(usage) != len(want) {
		t.Fatalf("usage = %+v", usage)
	}
	for index, usage := range usage {
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

// A request that waits for a slot longer than the server's write timeout
// still gets its answer.
func TestGatewayWaitOutlastsWriteTimeout(t *testing.T) {
	upstream := newFakeUpstream(t)
	store := &fakeStore{settings: []overload.ReviewSettings{{Name: "flint", Provider: overload.ProviderOpenAICompat, ConnectionKind: "local", BaseURL: upstream.URL + "/v1", Model: "flint", Concurrency: 1}}}
	gateway := New(store)
	server := httptest.NewUnstartedServer(gateway)
	server.Config.WriteTimeout = 200 * time.Millisecond
	server.Start()
	defer server.Close()
	release, err := gateway.queue(store.settings[0]).acquire(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		release()
	}()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"flint"}`))
	request.Header.Set("Authorization", "Bearer ovl_good")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("connection dropped while queued: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"hi"`) {
		t.Fatalf("queued call: %d %s", response.StatusCode, body)
	}
}

func TestGatewayDropsIdleQueues(t *testing.T) {
	flint := overload.ReviewSettings{Name: "flint", Concurrency: 1}
	gateway := New(&fakeStore{})
	release, _ := gateway.queue(overload.ReviewSettings{Name: "busy", Concurrency: 1}).acquire(context.Background(), "a")
	gateway.queue(overload.ReviewSettings{Name: "removed", Concurrency: 1})
	gateway.queue(flint)
	gateway.dropIdleQueues(map[string]overload.ReviewSettings{"flint": flint})
	kept := func() []string {
		gateway.mu.Lock()
		defer gateway.mu.Unlock()
		var names []string
		for name := range gateway.queues {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}
	if got := kept(); !slices.Equal(got, []string{"busy", "flint"}) {
		t.Fatalf("queues = %v", got)
	}
	release()
	gateway.dropIdleQueues(nil)
	if got := kept(); len(got) != 0 {
		t.Fatalf("idle queues kept: %v", got)
	}
}

func TestUsageFilter(t *testing.T) {
	for name, test := range map[string]struct {
		sse, drop bool
		chunks    []string
		want      string
		in, out   int64
	}{
		"split usage line": {
			sse: true, chunks: []string{"data: {\"usage\":{\"prompt_", "tokens\":4,\"completion_tokens\":2}}\n", "data: {\"usage\":{\"completion_tokens\":6}}"},
			want: "data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\ndata: {\"usage\":{\"completion_tokens\":6}}", in: 4, out: 6,
		},
		"drop usage chunk": {
			sse: true, drop: true, chunks: []string{"data: {\"choices\":[{}]}\n\ndata: {\"choices\":[],\"usa", "ge\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\r\n\r\ndata: [DONE]\n\n"},
			want: "data: {\"choices\":[{}]}\n\ndata: [DONE]\n\n", in: 3, out: 1,
		},
		"keep error chunk": {
			sse: true, drop: true, chunks: []string{"data: {\"error\":{\"message\":\"x\"}}\n\n"},
			want: "data: {\"error\":{\"message\":\"x\"}}\n\n",
		},
		"anthropic whole body": {
			chunks: []string{`{"usage":{"input_tokens":20,`, `"output_tokens":7}}`},
			want:   `{"usage":{"input_tokens":20,"output_tokens":7}}`, in: 20, out: 7,
		},
		"usage line inside a larger event": {
			sse: true, drop: true, chunks: []string{"event: usage\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3}}\n\n"},
			want: "event: usage\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":3}}\n\n", in: 3,
		},
		"usage line followed by another field": {
			sse: true, drop: true, chunks: []string{"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3}}\n", "id: 7\n\n"},
			want: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3}}\nid: 7\n\n", in: 3,
		},
		"usage line at the end of the stream": {
			sse: true, drop: true, chunks: []string{"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3}}\n"},
			want: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3}}\n", in: 3,
		},
		"line too long to inspect": {
			sse: true, drop: true, chunks: []string{"data: {\"choices\":[{\"delta\":\"" + strings.Repeat("x", maxObservedBytes), strings.Repeat("y", 10), "\"}]}\n\n", "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":9}}\n\n", "data: [DONE]\n\n"},
			want: "data: {\"choices\":[{\"delta\":\"" + strings.Repeat("x", maxObservedBytes) + strings.Repeat("y", 10) + "\"}]}\n\ndata: [DONE]\n\n", in: 5, out: 9,
		},
		"anthropic cached input": {
			chunks: []string{`{"usage":{"input_tokens":20,"cache_read_input_tokens":5000,"cache_creation_input_tokens":300,"output_tokens":7}}`},
			want:   `{"usage":{"input_tokens":20,"cache_read_input_tokens":5000,"cache_creation_input_tokens":300,"output_tokens":7}}`, in: 5320, out: 7,
		},
		"whole body too large to keep": {
			chunks: []string{`{"content":"` + strings.Repeat("x", maxObservedBytes), `","usage":{"prompt_tokens":40,"completion_tokens":12}}`},
			want:   `{"content":"` + strings.Repeat("x", maxObservedBytes) + `","usage":{"prompt_tokens":40,"completion_tokens":12}}`, in: 40, out: 12,
		},
	} {
		var count tokenCount
		filter := newUsageFilter(&count, test.sse, test.drop)
		var got strings.Builder
		for _, chunk := range test.chunks {
			got.Write(filter.Process([]byte(chunk)))
		}
		got.Write(filter.Finish())
		if got.String() != test.want || count.input != test.in || count.output != test.out {
			t.Errorf("%s: %q %+v", name, got.String(), count)
		}
	}
}
