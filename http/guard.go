package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// validForm parses a POST form and checks its CSRF token in constant time.
func validForm(w http.ResponseWriter, r *http.Request, csrf string) bool {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(csrf)) != 1 {
		http.Error(w, "Invalid form token", http.StatusForbidden)
		return false
	}
	return true
}

// hostPolicy lists the Host headers the UI answers to: loopback names and the
// host of OVERLOAD_BASE_URL. Rejecting other hosts stops DNS-rebinding pages
// from reaching a loopback UI as if they were same-origin.
type hostPolicy struct {
	allowed map[string]bool
}

func newHostPolicy(baseURL string) hostPolicy {
	policy := hostPolicy{allowed: map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}}
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Hostname() != "" {
		policy.allowed[strings.ToLower(parsed.Hostname())] = true
	}
	return policy
}

func hostOnly(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}

func (policy hostPolicy) allows(host string) bool {
	return policy.allowed[hostOnly(host)]
}

// sameOrigin rejects cross-site state changes. Browsers send Sec-Fetch-Site
// on every request; Origin is checked too for older clients.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host)
}

// oneTimeTokens issues short-lived single-use values, used as the GitHub App
// manifest state so the long-lived form token never leaves the server.
type oneTimeTokens struct {
	mu     sync.Mutex
	issued map[string]time.Time
	ttl    time.Duration
}

func newOneTimeTokens(ttl time.Duration) *oneTimeTokens {
	return &oneTimeTokens{issued: map[string]time.Time{}, ttl: ttl}
}

func (tokens *oneTimeTokens) issue() string {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("random source unavailable")
	}
	token := hex.EncodeToString(buf[:])
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	now := time.Now()
	for value, expires := range tokens.issued {
		if now.After(expires) {
			delete(tokens.issued, value)
		}
	}
	tokens.issued[token] = now.Add(tokens.ttl)
	return token
}

func (tokens *oneTimeTokens) consume(token string) bool {
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	expires, ok := tokens.issued[token]
	delete(tokens.issued, token)
	return ok && time.Now().Before(expires)
}
