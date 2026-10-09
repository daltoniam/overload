// Package modelproxy lets sandboxes call hosted models without holding
// their API keys. Overload grants a sandbox a short-lived URL for one model;
// the proxy adds the key and forwards only model requests for that model.
package modelproxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/daltoniam/overload"
)

// maxRequestBytes bounds one model request (prompt plus repository context).
const maxRequestBytes = 16 << 20

// allowedPaths are the model endpoints a sandbox may call.
var allowedPaths = map[string]bool{"chat/completions": true, "responses": true}

type grant struct {
	model   overload.ModelProfile
	expires time.Time
}

// Proxy forwards granted model requests. It is safe for concurrent use.
type Proxy struct {
	base   string
	client *http.Client
	now    func() time.Time

	mu     sync.Mutex
	grants map[string]grant
}

// New returns a proxy whose grants are reachable at base (for example
// http://overload.overload.svc:8083).
func New(base string) *Proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Proxy{base: strings.TrimRight(base, "/"), client: &http.Client{Transport: transport}, now: time.Now, grants: map[string]grant{}}
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
		if now.After(entry.expires) {
			delete(proxy.grants, existing)
		}
	}
	proxy.grants[token] = grant{model: model, expires: now.Add(ttl)}
	proxy.mu.Unlock()
	return proxy.base + "/v1/m/" + token, func() {
		proxy.mu.Lock()
		delete(proxy.grants, token)
		proxy.mu.Unlock()
	}
}

func (proxy *Proxy) lookup(token string) (overload.ModelProfile, bool) {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	entry, ok := proxy.grants[token]
	if !ok || proxy.now().After(entry.expires) {
		return overload.ModelProfile{}, false
	}
	return entry.model, true
}

func (proxy *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/m/")
	token, path, found := strings.Cut(rest, "/")
	if !ok || !found || r.Method != http.MethodPost || !allowedPaths[path] || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	model, ok := proxy.lookup(token)
	if !ok {
		http.Error(w, "unknown or expired model grant", http.StatusUnauthorized)
		return
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
	if model.APIKeyEnv != "" {
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
	buf := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buf)
		if n > 0 {
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
