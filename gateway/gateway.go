// Package gateway serves saved models to agents and CLIs through OpenAI and
// Anthropic compatible endpoints. Callers authenticate with per-person API
// keys; the model's own API key never leaves overload. Requests for one
// model share its parallel slots fairly, and every call is recorded in
// tokens and minutes.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
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

// Authenticator turns a request's credentials into a caller allowed to
// call models; ok is false when the request carries none it recognizes.
type Authenticator func(r *http.Request) (caller Caller, ok bool, err error)

const (
	maxRequestBytes = 16 << 20
	// maxWaiting bounds the requests waiting for one model.
	maxWaiting = 64
	// Calls are bounded like review calls: local models can think for a
	// long time.
	localCallLimit  = 90 * time.Minute
	hostedCallLimit = 15 * time.Minute
)

// Gateway is an http.Handler for /v1/models, /v1/chat/completions and
// /v1/messages.
type Gateway struct {
	store  Store
	client *http.Client
	extra  []Authenticator

	mu     sync.Mutex
	queues map[string]*fairQueue
}

// New returns a gateway over store. extra authenticators are tried before
// API keys.
func New(store Store, extra ...Authenticator) *Gateway {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 0
	return &Gateway{store: store, client: &http.Client{Transport: transport}, extra: extra, queues: map[string]*fairQueue{}}
}

func (gateway *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	caller, err := gateway.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication_error", err.Error())
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
func bearer(r *http.Request) string {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func (gateway *Gateway) authenticate(r *http.Request) (Caller, error) {
	for _, authenticator := range gateway.extra {
		caller, ok, err := authenticator(r)
		if err != nil {
			return Caller{}, err
		}
		if ok {
			return caller, nil
		}
	}
	secret := bearer(r)
	if secret == "" {
		return Caller{}, errors.New("send an overload API key as a bearer token or x-api-key")
	}
	key, user, err := gateway.store.AuthenticateAPIKey(r.Context(), secret)
	if err != nil {
		return Caller{}, errors.New("invalid, expired or revoked API key")
	}
	if !key.HasScope(overload.ScopeModels) {
		return Caller{}, errors.New("this API key cannot call models")
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
		queue = newFairQueue(setting.Concurrency, maxWaiting)
		gateway.queues[setting.Name] = queue
	}
	queue.setSlots(setting.Concurrency)
	return queue
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
	var stream bool
	_ = json.Unmarshal(request["stream"], &stream)
	if stream && provider == overload.ProviderOpenAICompat {
		options := map[string]json.RawMessage{}
		_ = json.Unmarshal(request["stream_options"], &options)
		if _, set := options["include_usage"]; !set {
			options["include_usage"] = json.RawMessage("true")
		}
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
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(limit))
	usage := overload.Usage{KeyID: caller.KeyID, UserID: caller.UserID, Model: name, Endpoint: r.URL.Path, QueueTime: queued}
	status, counted := gateway.call(ctx, w, r, setting, body)
	usage.Status, usage.InputTokens, usage.OutputTokens, usage.Duration = status, counted.input, counted.output, time.Since(started)-queued
	if err := gateway.store.RecordUsage(context.WithoutCancel(r.Context()), usage); err != nil {
		slog.Warn("gateway usage not recorded", "model", name, "error", err)
	}
}

// call sends the request upstream with the model's credentials and copies
// the answer back, counting tokens on the way. It returns the status sent.
func (gateway *Gateway) call(ctx context.Context, w http.ResponseWriter, r *http.Request, setting overload.ReviewSettings, body []byte) (int, tokenCount) {
	target := strings.TrimRight(setting.BaseURL, "/") + "/chat/completions"
	if setting.Provider == overload.ProviderAnthropic {
		target = strings.TrimRight(setting.BaseURL, "/") + "/v1/messages"
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "invalid model URL")
		return http.StatusBadGateway, tokenCount{}
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("User-Agent", "overload-gateway")
	if accept := r.Header.Get("Accept"); accept != "" {
		upstream.Header.Set("Accept", accept)
	}
	key := ""
	if setting.APIKeyEnv != "" {
		key = os.Getenv(setting.APIKeyEnv)
	}
	if setting.Provider == overload.ProviderAnthropic {
		version := r.Header.Get("Anthropic-Version")
		if version == "" {
			version = "2023-06-01"
		}
		upstream.Header.Set("Anthropic-Version", version)
		if beta := r.Header.Get("Anthropic-Beta"); beta != "" {
			upstream.Header.Set("Anthropic-Beta", beta)
		}
		if key != "" {
			upstream.Header.Set("X-Api-Key", key)
		}
	} else if key != "" {
		upstream.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := gateway.client.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "the model server did not answer")
		return http.StatusBadGateway, tokenCount{}
	}
	defer func() { _ = response.Body.Close() }()
	for _, name := range []string{"Content-Type", "Retry-After"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	var counted tokenCount
	sse := strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream")
	scanner := &lineScanner{observe: counted.observe, sse: sse}
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			scanner.write(buffer[:n])
			if _, err := w.Write(buffer[:n]); err != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
	scanner.finish()
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

// lineScanner feeds response JSON to observe: every `data:` line of an
// event stream, or the whole body otherwise (up to 8 MiB).
type lineScanner struct {
	observe func([]byte)
	sse     bool
	pending bytes.Buffer
}

const maxObservedBytes = 8 << 20

func (scanner *lineScanner) write(data []byte) {
	if scanner.pending.Len()+len(data) > maxObservedBytes {
		if !scanner.sse {
			return
		}
		scanner.pending.Reset()
	}
	scanner.pending.Write(data)
	if !scanner.sse {
		return
	}
	for {
		line, err := scanner.pending.ReadBytes('\n')
		if err != nil {
			scanner.pending.Reset()
			scanner.pending.Write(line)
			return
		}
		scanner.line(line)
	}
}

func (scanner *lineScanner) line(line []byte) {
	if payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
		scanner.observe(bytes.TrimSpace(payload))
	}
}

func (scanner *lineScanner) finish() {
	if !scanner.sse {
		scanner.observe(scanner.pending.Bytes())
		return
	}
	reader := bufio.NewScanner(&scanner.pending)
	for reader.Scan() {
		scanner.line(reader.Bytes())
	}
}

// writeError answers in a shape both OpenAI and Anthropic clients read.
func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
}
