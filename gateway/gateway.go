// Package gateway serves saved models to agents and CLIs through OpenAI and
// Anthropic compatible endpoints. Callers authenticate with per-person API
// keys; the model's own API key never leaves overload. Requests for one
// model share its parallel slots fairly, and every forwarded call is
// recorded in tokens and minutes.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/modelproxy"
)

// Store is what the gateway needs from the database.
type Store interface {
	AuthenticateAPIKey(ctx context.Context, secret string) (overload.APIKey, *overload.User, error)
	ListReviewSettings(context.Context) ([]overload.ReviewSettings, error)
	LoadModelPolicy(context.Context) (catalog.Policy, error)
	RecordUsage(context.Context, overload.Usage) error
}

// Caller is who a gateway request is for.
type Caller struct {
	KeyID  int64
	UserID int64
	// Name identifies the caller for fair queueing and logs.
	Name string
}

const (
	maxRequestBytes = 16 << 20
	// maxWaiting bounds the requests waiting for one model, and
	// maxWaitingPerCaller the share of them one caller may hold, so one
	// agent cannot fill the queue and turn everyone else away.
	maxWaiting          = 64
	maxWaitingPerCaller = 16
	// Calls are bounded like review calls: local models can think for a
	// long time.
	localCallLimit  = 90 * time.Minute
	hostedCallLimit = 15 * time.Minute
	// defaultAnthropicVersion is sent when an Anthropic client names none.
	defaultAnthropicVersion = "2023-06-01"
)

// Gateway is an http.Handler for /v1/models, /v1/chat/completions and
// /v1/messages.
type Gateway struct {
	store    Store
	upstream *modelproxy.Upstream

	mu     sync.Mutex
	queues map[string]*fairQueue
}

// New returns a gateway over store.
func New(store Store) *Gateway {
	return &Gateway{store: store, upstream: modelproxy.NewUpstream("overload-gateway"), queues: map[string]*fairQueue{}}
}

func (gateway *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	caller, denied := gateway.authenticate(r)
	if denied != nil {
		writeError(w, denied.status, denied.kind, denied.message)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		gateway.listModels(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		gateway.forward(w, r, caller, overload.ProviderOpenAICompat)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		gateway.forward(w, r, caller, overload.ProviderAnthropic)
	default:
		writeError(w, http.StatusNotFound, "not_found_error", "unknown endpoint; overload serves /v1/models, /v1/chat/completions and /v1/messages")
	}
}

// bearer returns the credential from Authorization: Bearer or x-api-key.
// The scheme is case-insensitive, as HTTP requires.
func bearer(r *http.Request) string {
	if scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(token)
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// denial is a request the gateway refuses before calling a model.
type denial struct {
	status        int
	kind, message string
}

func (gateway *Gateway) authenticate(r *http.Request) (Caller, *denial) {
	secret := bearer(r)
	if secret == "" {
		return Caller{}, &denial{http.StatusUnauthorized, "authentication_error", "send an overload API key as a bearer token or x-api-key"}
	}
	key, user, err := gateway.store.AuthenticateAPIKey(r.Context(), secret)
	if errors.Is(err, overload.ErrInvalidAPIKey) {
		return Caller{}, &denial{http.StatusUnauthorized, "authentication_error", "invalid, expired or revoked API key"}
	}
	if err != nil {
		slog.Warn("gateway key check failed", "error", err)
		return Caller{}, &denial{http.StatusServiceUnavailable, "api_error", "unable to check the API key; retry later"}
	}
	if !key.HasScope(overload.ScopeModels) {
		return Caller{}, &denial{http.StatusForbidden, "permission_error", "this API key cannot call models"}
	}
	if user != nil && !overload.RoleAtLeast(user.Role, overload.RoleMember) {
		return Caller{}, &denial{http.StatusForbidden, "permission_error", "the owner of this API key is not allowed to use models"}
	}
	caller := Caller{KeyID: key.ID, Name: "key:" + key.Prefix}
	if user != nil {
		caller.UserID, caller.Name = user.ID, "user:"+fmt.Sprint(user.ID)
	}
	return caller, nil
}

// available returns the saved models the policy allows, by name.
func (gateway *Gateway) available(ctx context.Context) (map[string]overload.ReviewSettings, error) {
	settings, err := gateway.store.ListReviewSettings(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := gateway.store.LoadModelPolicy(ctx)
	if err != nil {
		return nil, err
	}
	models := map[string]overload.ReviewSettings{}
	for _, setting := range settings {
		if policy.Check(setting.Catalog, setting.Model).Allowed {
			models[setting.Name] = setting
		}
	}
	return models, nil
}

func (gateway *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := gateway.available(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "unable to list models")
		return
	}
	type entry struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
		OwnedBy     string `json:"owned_by"`
		Created     int64  `json:"created"`
		API         string `json:"api"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []entry `json:"data"`
	}{Object: "list", Data: []entry{}}
	for name, setting := range models {
		// The gateway serves Chat Completions for every OpenAI-compatible
		// model, including those reviews call through the Responses API.
		api := "chat_completions"
		if setting.Provider == overload.ProviderAnthropic {
			api = "messages"
		}
		list.Data = append(list.Data, entry{ID: name, Object: "model", Type: "model", DisplayName: name, OwnedBy: "overload", API: api})
	}
	slices.SortFunc(list.Data, func(a, b entry) int { return strings.Compare(a.ID, b.ID) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (gateway *Gateway) queue(setting overload.ReviewSettings) *fairQueue {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	queue, ok := gateway.queues[setting.Name]
	if !ok {
		queue = newFairQueue(setting.Concurrency, maxWaiting, maxWaitingPerCaller)
		gateway.queues[setting.Name] = queue
	}
	queue.setSlots(setting.Concurrency)
	return queue
}

// dropIdleQueues forgets the queues of models no longer available once no
// request holds or waits for them.
func (gateway *Gateway) dropIdleQueues(models map[string]overload.ReviewSettings) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	for name, queue := range gateway.queues {
		if _, ok := models[name]; !ok && queue.idle() {
			delete(gateway.queues, name)
		}
	}
}

func (gateway *Gateway) forward(w http.ResponseWriter, r *http.Request, caller Caller, provider string) {
	started := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil || len(body) > maxRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request larger than 16 MiB")
		return
	}
	var request map[string]json.RawMessage
	var name string
	if json.Unmarshal(body, &request) != nil || json.Unmarshal(request["model"], &name) != nil || name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the body must be a JSON object naming a model")
		return
	}
	models, err := gateway.available(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "unable to load models")
		return
	}
	gateway.dropIdleQueues(models)
	setting, ok := models[name]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("no model named %q is available; see GET /v1/models", name))
		return
	}
	if (setting.Provider == overload.ProviderAnthropic) != (provider == overload.ProviderAnthropic) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q uses the other API; call /v1/messages for Anthropic models and /v1/chat/completions for the rest", name))
		return
	}
	request["model"], _ = json.Marshal(setting.Model)
	var stream, clientWantsUsage bool
	_ = json.Unmarshal(request["stream"], &stream)
	if stream && provider == overload.ProviderOpenAICompat {
		// Usage is always requested so every call is counted; the extra
		// chunk is dropped again for clients that did not ask for it.
		options := map[string]json.RawMessage{}
		_ = json.Unmarshal(request["stream_options"], &options)
		_ = json.Unmarshal(options["include_usage"], &clientWantsUsage)
		options["include_usage"] = json.RawMessage("true")
		request["stream_options"], _ = json.Marshal(options)
	}
	body, err = json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid request")
		return
	}
	limit := localCallLimit
	if setting.ConnectionKind == "hosted" {
		limit = hostedCallLimit
	}
	// The server's write timeout is shorter than a wait for a busy model,
	// so extend it before queueing, not after.
	_ = http.NewResponseController(w).SetWriteDeadline(started.Add(limit))
	ctx, cancel := context.WithTimeout(r.Context(), limit)
	defer cancel()
	release, err := gateway.queue(setting).acquire(ctx, caller.Name)
	if errors.Is(err, errQueueFull) {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "too many requests are waiting for this model; retry later")
		return
	}
	if err != nil {
		writeError(w, http.StatusGatewayTimeout, "timeout_error", "gave up waiting for a free slot")
		return
	}
	defer release()
	queued := time.Since(started)
	usage := overload.Usage{KeyID: caller.KeyID, UserID: caller.UserID, Model: name, Endpoint: r.URL.Path, QueueTime: queued}
	status, counted := gateway.call(ctx, w, r, setting, body, stream && provider == overload.ProviderOpenAICompat && !clientWantsUsage)
	usage.Status, usage.InputTokens, usage.OutputTokens, usage.Duration = status, counted.input, counted.output, time.Since(started)-queued
	if err := gateway.store.RecordUsage(context.WithoutCancel(r.Context()), usage); err != nil {
		slog.Warn("gateway usage not recorded", "model", name, "error", err)
	}
}

// call sends the request upstream with the model's credentials and copies
// the answer back, counting tokens on the way and dropping the usage-only
// stream chunk when dropUsage is set. It returns the status sent.
func (gateway *Gateway) call(ctx context.Context, w http.ResponseWriter, r *http.Request, setting overload.ReviewSettings, body []byte, dropUsage bool) (int, tokenCount) {
	path := "chat/completions"
	headers := http.Header{}
	if accept := r.Header.Get("Accept"); accept != "" {
		headers.Set("Accept", accept)
	}
	if setting.Provider == overload.ProviderAnthropic {
		path = "v1/messages"
		version := r.Header.Get("Anthropic-Version")
		if version == "" {
			version = defaultAnthropicVersion
		}
		headers.Set("Anthropic-Version", version)
	}
	response, err := gateway.upstream.Send(ctx, setting.Profile(), path, headers, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "the model server did not answer")
		return http.StatusBadGateway, tokenCount{}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		// Passed through, these would look like a bad overload key.
		slog.Warn("model server rejected overload's credentials", "model", setting.Name, "status", response.StatusCode)
		writeError(w, http.StatusBadGateway, "api_error", "the model server rejected overload's credentials for this model; ask an administrator to check its API key")
		return http.StatusBadGateway, tokenCount{}
	}
	var counted tokenCount
	filter := &usageFilter{count: &counted, sse: strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream"), dropUsageChunk: dropUsage}
	modelproxy.Relay(w, response, response.StatusCode, filter)
	return response.StatusCode, counted
}

// tokenCount collects token usage from OpenAI and Anthropic responses,
// whole or streamed.
type tokenCount struct {
	input, output int64
}

type usageFields struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

// observe reads usage from one JSON response or event: OpenAI's usage
// (prompt and completion tokens), Anthropic's usage, and the usage inside
// Anthropic's message_start event. Streamed counts are cumulative, so the
// largest seen wins.
func (count *tokenCount) observe(data []byte) {
	var event struct {
		Usage   *usageFields `json:"usage"`
		Message *struct {
			Usage *usageFields `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	usages := []*usageFields{event.Usage}
	if event.Message != nil {
		usages = append(usages, event.Message.Usage)
	}
	for _, usage := range usages {
		if usage == nil {
			continue
		}
		count.input = max(count.input, usage.PromptTokens, usage.InputTokens)
		count.output = max(count.output, usage.CompletionTokens, usage.OutputTokens)
	}
}

const maxObservedBytes = 8 << 20

// usageFilter passes a model response through while feeding its JSON to a
// tokenCount: every `data:` line of an event stream, or the whole body
// otherwise (up to 8 MiB). When dropUsageChunk is set it removes the
// OpenAI stream chunk that carries only usage.
type usageFilter struct {
	count          *tokenCount
	sse            bool
	dropUsageChunk bool
	pending        bytes.Buffer
	overflowed     bool
	skipBlank      bool
}

func (filter *usageFilter) Process(chunk []byte) []byte {
	if !filter.sse {
		if filter.overflowed || filter.pending.Len()+len(chunk) > maxObservedBytes {
			filter.overflowed = true
		} else {
			filter.pending.Write(chunk)
		}
		return chunk
	}
	filter.pending.Write(chunk)
	var out []byte
	for {
		index := bytes.IndexByte(filter.pending.Bytes(), '\n')
		if index < 0 {
			break
		}
		out = filter.line(out, filter.pending.Next(index+1))
	}
	if filter.pending.Len() > maxObservedBytes {
		out = append(out, filter.pending.Bytes()...)
		filter.pending.Reset()
	}
	return out
}

func (filter *usageFilter) Finish() []byte {
	if !filter.sse {
		if !filter.overflowed {
			filter.count.observe(filter.pending.Bytes())
		}
		filter.pending.Reset()
		return nil
	}
	rest := bytes.Clone(filter.pending.Bytes())
	filter.pending.Reset()
	if len(rest) == 0 {
		return nil
	}
	return filter.line(nil, rest)
}

// line handles one event stream line, appending what to send to out.
func (filter *usageFilter) line(out, line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 && filter.skipBlank {
		filter.skipBlank = false
		return out
	}
	filter.skipBlank = false
	if payload, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
		payload = bytes.TrimSpace(payload)
		filter.count.observe(payload)
		if filter.dropUsageChunk && usageOnly(payload) {
			filter.skipBlank = true
			return out
		}
	}
	return append(out, line...)
}

// usageOnly reports whether an OpenAI stream chunk carries usage and no
// choices: the chunk include_usage adds at the end of a stream.
func usageOnly(payload []byte) bool {
	var chunk struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if json.Unmarshal(payload, &chunk) != nil {
		return false
	}
	return len(chunk.Choices) == 0 && len(chunk.Usage) > 0 && string(chunk.Usage) != "null"
}

// writeError answers in a shape both OpenAI and Anthropic clients read.
func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
}
