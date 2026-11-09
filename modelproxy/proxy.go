// Package modelproxy lets sandboxes call hosted models without holding
// their API keys. Overload grants a sandbox a short-lived URL for one model;
// the proxy adds the key and forwards only model requests for that model.
package modelproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/daltoniam/overload"
)

// maxRequestBytes bounds one model request (prompt plus repository context).
const maxRequestBytes = 16 << 20

// allowedPaths are the model endpoints a sandbox may call, by provider.
// OpenAI-compatible base URLs end in /v1; Anthropic's do not.
var allowedPaths = map[string]map[string]bool{
	overload.ProviderOpenAICompat: {"chat/completions": true, "responses": true},
	overload.ProviderAnthropic:    {"v1/messages": true},
}

// anthropicHeader matches the version and beta headers the Anthropic SDK
// sends, which the proxy passes through.
var anthropicHeader = regexp.MustCompile(`^[A-Za-z0-9._,-]{1,200}$`)

type grant struct {
	model   overload.ModelProfile
	expires time.Time
	// ended is when the grant was revoked or expired; it stops
	// authorizing calls but keeps counting its spend for settleWindow.
	ended time.Time
	// spent is what the grant's calls cost so far, including calls that
	// finish after the grant ended. It counts as pending spend until the
	// run has recorded its cost.
	spent float64
}

// settleWindow is how long a finished grant's spend keeps counting as
// pending: long enough for its run to record the cost and for the budget
// gate to read it again.
const settleWindow = 2 * time.Minute

func (entry *grant) live(now time.Time) bool {
	return entry.ended.IsZero() && !now.After(entry.expires)
}

// endedAt is when the grant stopped authorizing calls.
func (entry *grant) endedAt() time.Time {
	if !entry.ended.IsZero() {
		return entry.ended
	}
	return entry.expires
}

// BudgetGate decides whether more model calls may go out: pending is what
// calls through the proxy cost that runs have not recorded yet. It returns
// a reason when the budget is spent.
type BudgetGate func(ctx context.Context, pending float64) (string, error)

// maxMeteredBytes bounds the copy of an answer kept to read its usage.
const maxMeteredBytes = 16 << 20

// Proxy forwards granted model requests and meters what they cost. It is
// safe for concurrent use.
type Proxy struct {
	base   string
	client *http.Client
	now    func() time.Time
	// Budget, when set, refuses calls with 429 once the budget is spent:
	// the backstop for reviews already running when it ran out.
	Budget BudgetGate

	mu     sync.Mutex
	grants map[string]*grant
}

// New returns a proxy whose grants are reachable at base (for example
// http://overload.overload.svc:8083).
func New(base string) *Proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Proxy{base: strings.TrimRight(base, "/"), client: &http.Client{Transport: transport}, now: time.Now, grants: map[string]*grant{}}
}

// Grant returns a base URL that reaches model for ttl, and a function that
// revokes it early.
func (proxy *Proxy) Grant(model overload.ModelProfile, ttl time.Duration) (string, func()) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("random source unavailable")
	}
	token := hex.EncodeToString(buf[:])
	proxy.mu.Lock()
	now := proxy.now()
	for existing, entry := range proxy.grants {
		if !entry.live(now) && now.Sub(entry.endedAt()) > settleWindow {
			delete(proxy.grants, existing)
		}
	}
	proxy.grants[token] = &grant{model: model, expires: now.Add(ttl)}
	proxy.mu.Unlock()
	return proxy.base + "/v1/m/" + token, func() {
		proxy.mu.Lock()
		if entry, ok := proxy.grants[token]; ok && entry.ended.IsZero() {
			entry.ended = proxy.now()
		}
		proxy.mu.Unlock()
	}
}

func (proxy *Proxy) lookup(token string) (overload.ModelProfile, bool) {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	entry, ok := proxy.grants[token]
	if !ok || !entry.live(proxy.now()) {
		return overload.ModelProfile{}, false
	}
	return entry.model, true
}

// Pending is what calls through live grants, and grants that ended within
// settleWindow, cost so far.
func (proxy *Proxy) Pending() float64 {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	now := proxy.now()
	total := 0.0
	for _, entry := range proxy.grants {
		if entry.live(now) || now.Sub(entry.endedAt()) <= settleWindow {
			total += entry.spent
		}
	}
	return total
}

func (proxy *Proxy) charge(token string, usd float64) {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if entry, ok := proxy.grants[token]; ok {
		entry.spent += usd
	}
}

func (proxy *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/m/")
	token, path, found := strings.Cut(rest, "/")
	if !ok || !found || r.Method != http.MethodPost || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	model, ok := proxy.lookup(token)
	if !ok {
		http.Error(w, "unknown or expired model grant", http.StatusUnauthorized)
		return
	}
	provider := model.Provider
	if provider == "" {
		provider = overload.ProviderOpenAICompat
	}
	if !allowedPaths[provider][path] {
		http.NotFound(w, r)
		return
	}
	if proxy.Budget != nil {
		reason, err := proxy.Budget(r.Context(), proxy.Pending())
		if err != nil {
			slog.Warn("model proxy could not check the budget", "error", err)
		}
		if reason != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"error":{"type":"budget_exhausted","message":"overload's %s review budget is spent"}}`, reason)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil || len(body) > maxRequestBytes {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var request struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &request) != nil || request.Model != model.Model {
		http.Error(w, "request must name the granted model", http.StatusForbidden)
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(model.BaseURL, "/")+"/"+path, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "invalid model URL", http.StatusBadGateway)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	if accept := r.Header.Get("Accept"); accept != "" {
		upstream.Header.Set("Accept", accept)
	}
	upstream.Header.Set("User-Agent", "overload-model-proxy")
	for name, value := range model.Headers {
		upstream.Header.Set(name, value)
	}
	switch {
	case provider == overload.ProviderAnthropic:
		for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
			if value := r.Header.Get(name); anthropicHeader.MatchString(value) {
				upstream.Header.Set(name, value)
			}
		}
		if model.APIKeyEnv != "" {
			upstream.Header.Set("X-Api-Key", os.Getenv(model.APIKeyEnv))
		}
	case model.APIKeyEnv != "":
		upstream.Header.Set("Authorization", "Bearer "+os.Getenv(model.APIKeyEnv))
	}
	response, err := proxy.client.Do(upstream)
	if err != nil {
		http.Error(w, "model request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	for _, name := range []string{"Content-Type", "Retry-After"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	metered := newUsageMeter(provider, response.Header.Get("Content-Type"))
	defer func() {
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return
		}
		usage, exact := metered.usage()
		if !exact {
			slog.Warn("model proxy could not read a call's usage; charging an estimate", "model", model.Model, "response_bytes", metered.bytes)
		}
		if usd, priced := model.Pricing.Cost(usage); priced {
			proxy.charge(token, usd)
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buf)
		if n > 0 {
			metered.write(buf[:n])
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}
