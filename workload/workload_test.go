package workload

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/daltoniam/overload"
)

// countingRules is a rule store that counts reads and can fail them.
type countingRules struct {
	rules []overload.WorkloadRule
	reads atomic.Int64
	fail  atomic.Bool
}

func (store *countingRules) ListWorkloadRules(context.Context) ([]overload.WorkloadRule, error) {
	store.reads.Add(1)
	if store.fail.Load() {
		return nil, errors.New(`relation "workload_rules" does not exist`)
	}
	return store.rules, nil
}

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

// fakeIssuer is a fake OIDC issuer for workload tokens.
type fakeIssuer struct {
	URL       string
	transport http.RoundTripper
	key       *rsa.PrivateKey
	discovery atomic.Int64
	keys      atomic.Int64
	broken    atomic.Bool
	// keysBroken fails only the key set, not discovery.
	keysBroken atomic.Bool
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	issuer := &fakeIssuer{key: newKey(t)}
	discovery := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: issuer.key.Public(), KeyID: "k", Algorithm: "RS256"}}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/keys" {
			issuer.keys.Add(1)
		} else {
			issuer.discovery.Add(1)
		}
		if issuer.broken.Load() || r.URL.Path == "/keys" && issuer.keysBroken.Load() {
			http.Error(w, "internal detail: upstream 10.0.0.7 refused", http.StatusInternalServerError)
			return
		}
		discovery.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	discovery.SetIssuer(server.URL)
	issuer.URL, issuer.transport = server.URL, server.Client().Transport
	return issuer
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func (issuer *fakeIssuer) requests() int64 { return issuer.discovery.Load() + issuer.keys.Load() }

// token signs a token with extra claims, valid from now for lifetime.
func (issuer *fakeIssuer) token(audience, subject, claims string, now time.Time, lifetime time.Duration) string {
	return issuer.signedBy(issuer.key, audience, subject, claims, now, lifetime)
}

func (issuer *fakeIssuer) signedBy(key *rsa.PrivateKey, audience, subject, claims string, now time.Time, lifetime time.Duration) string {
	body := fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":%q,"iat":%d,"exp":%d%s}`, issuer.URL, audience, subject, now.Unix(), now.Add(lifetime).Unix(), claims)
	return oidctest.SignIDToken(key, "k", "RS256", body)
}

const audience = "https://overload.example.com"

func newTestVerifier(store RuleStore, transport http.RoundTripper) (*Verifier, *clock) {
	c := &clock{now: time.Now()}
	verifier := NewVerifier(store, transport)
	verifier.now = c.Now
	return verifier, c
}

func TestVerify(t *testing.T) {
	issuer := newFakeIssuer(t)
	store := &countingRules{rules: []overload.WorkloadRule{
		{Name: "ci", Issuer: issuer.URL, Audience: audience, Conditions: map[string]string{"repository": "acme/*", "ref": "refs/heads/main"}, Scopes: []string{overload.ScopeReviews}, Enabled: true},
		{Name: "models", Issuer: issuer.URL, Audience: audience, Conditions: map[string]string{"repository": "acme/agents"}, Scopes: []string{overload.ScopeModels}, Enabled: true},
		{Name: "off", Issuer: issuer.URL, Audience: audience, Conditions: map[string]string{"repository": "*/*"}, Scopes: []string{overload.ScopeModels}, Enabled: false},
	}}
	verifier, c := newTestVerifier(store, issuer.transport)
	ctx := context.Background()
	now := c.Now()

	identity, err := verifier.Verify(ctx, issuer.token(audience, "repo:acme/agents:ref:refs/heads/main", `,"repository":"acme/agents","ref":"refs/heads/main"`, now, time.Minute))
	if err != nil || !identity.HasScope(overload.ScopeReviews) || !identity.HasScope(overload.ScopeModels) || identity.Name() != "workload:ci,models acme/agents" || identity.Label() != "ci,models acme/agents" {
		t.Fatalf("identity = %+v, %v", identity, err)
	}
	other, err := verifier.Verify(ctx, issuer.token(audience, "repo:acme/agents:environment:prod", `,"repository":"acme/agents","ref":"refs/heads/main"`, now, time.Minute))
	if err != nil || other.Name() != identity.Name() {
		t.Fatalf("another subject from the same repository has a different identity: %q, %v", other.Name(), err)
	}
	for name, token := range map[string]string{
		"wrong branch":   issuer.token(audience, "s", `,"repository":"acme/web","ref":"refs/heads/feature"`, now, time.Minute),
		"wrong audience": issuer.token("https://elsewhere.example.com", "s", `,"repository":"acme/web","ref":"refs/heads/main"`, now, time.Minute),
		"expired":        issuer.token(audience, "s", `,"repository":"acme/web","ref":"refs/heads/main"`, now, -time.Minute),
		"non-string":     issuer.token(audience, "s", `,"repository":["acme/web"],"ref":"refs/heads/main"`, now, time.Minute),
		"unknown key":    issuer.signedBy(newKey(t), audience, "s", `,"repository":"acme/web","ref":"refs/heads/main"`, now, time.Minute),
	} {
		if _, err := verifier.Verify(ctx, token); !errors.Is(err, ErrRejected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := verifier.Verify(ctx, "ovl_key"); !errors.Is(err, ErrNotWorkloadToken) {
		t.Fatalf("API key: %v", err)
	}
	before := issuer.requests()
	untrusted := newFakeIssuer(t)
	if _, err := verifier.Verify(ctx, untrusted.token(audience, "s", `,"repository":"acme/web"`, now, time.Minute)); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "no workload rule trusts") {
		t.Fatalf("untrusted issuer: %v", err)
	}
	if untrusted.requests() != 0 || issuer.requests() != before {
		t.Fatal("an untrusted issuer was contacted")
	}
}

func TestVerifierContactsIssuersSparingly(t *testing.T) {
	issuer := newFakeIssuer(t)
	store := &countingRules{rules: []overload.WorkloadRule{{Name: "ci", Issuer: issuer.URL, Audience: audience, Conditions: map[string]string{"repository": "acme/api"}, Scopes: []string{overload.ScopeReviews}, Enabled: true}}}
	verifier, c := newTestVerifier(store, issuer.transport)
	ctx := context.Background()
	good := func() string { return issuer.token(audience, "s", `,"repository":"acme/api"`, c.Now(), time.Hour) }
	forged := func() string {
		return issuer.signedBy(newKey(t), audience, "s", `,"repository":"acme/api"`, c.Now(), time.Hour)
	}

	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			if _, err := verifier.Verify(ctx, good()); err != nil {
				t.Error(err)
			}
		})
	}
	wait.Wait()
	if issuer.discovery.Load() != 1 || issuer.keys.Load() != 1 {
		t.Fatalf("concurrent first tokens: %d discovery, %d key fetches", issuer.discovery.Load(), issuer.keys.Load())
	}
	if store.reads.Load() != 1 {
		t.Fatalf("rules read %d times within the cache period", store.reads.Load())
	}

	for range 5 {
		if _, err := verifier.Verify(ctx, forged()); !errors.Is(err, ErrRejected) {
			t.Fatalf("forged token: %v", err)
		}
	}
	if issuer.keys.Load() != 1 {
		t.Fatalf("forged tokens fetched keys %d times within a minute", issuer.keys.Load())
	}
	c.advance(keyRefetchInterval)
	for range 5 {
		_, _ = verifier.Verify(ctx, forged())
	}
	if issuer.keys.Load() != 2 {
		t.Fatalf("after a minute, forged tokens fetched keys %d times; want one more", issuer.keys.Load())
	}
	if _, err := verifier.Verify(ctx, good()); err != nil {
		t.Fatalf("a good token after forged ones: %v", err)
	}
	if store.reads.Load() != 2 {
		t.Fatalf("rules read %d times; want once more after the cache period", store.reads.Load())
	}
}

func TestVerifierRemembersUnavailableIssuers(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.broken.Store(true)
	store := &countingRules{rules: []overload.WorkloadRule{{Name: "k8s", Issuer: issuer.URL, Audience: audience, Conditions: map[string]string{"sub": "system:serviceaccount:ci:*"}, Scopes: []string{overload.ScopeModels}, Enabled: true}}}
	verifier, c := newTestVerifier(store, issuer.transport)
	ctx := context.Background()
	token := func() string {
		return issuer.token(audience, "system:serviceaccount:ci:runner", "", c.Now(), time.Hour)
	}

	for range 3 {
		_, err := verifier.Verify(ctx, token())
		if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "10.0.0.7") {
			t.Fatalf("broken issuer: %v", err)
		}
	}
	if issuer.discovery.Load() != 1 {
		t.Fatalf("a failing issuer was asked %d times within %s", issuer.discovery.Load(), discoveryRetry)
	}
	issuer.broken.Store(false)
	c.advance(discoveryRetry)
	if _, err := verifier.Verify(ctx, token()); err != nil {
		t.Fatalf("after the issuer recovered: %v", err)
	}

	store.fail.Store(true)
	verifier.Forget()
	if _, err := verifier.Verify(ctx, token()); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "relation") {
		t.Fatalf("unreadable rules: %v", err)
	}
}

func TestVerifierForgetsChangedRules(t *testing.T) {
	issuer := newFakeIssuer(t)
	rule := overload.WorkloadRule{Name: "ci", Issuer: issuer.URL, Audience: audience, Conditions: map[string]string{"repository": "acme/api"}, Scopes: []string{overload.ScopeReviews}, Enabled: true}
	store := &countingRules{rules: []overload.WorkloadRule{rule}}
	verifier, c := newTestVerifier(store, issuer.transport)
	token := issuer.token(audience, "s", `,"repository":"acme/api"`, c.Now(), time.Hour)
	if _, err := verifier.Verify(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	store.rules = nil
	verifier.Forget()
	if _, err := verifier.Verify(context.Background(), token); !errors.Is(err, ErrRejected) {
		t.Fatalf("a deleted rule still trusted the token: %v", err)
	}
}

func TestIssuerClient(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) }))
	defer plain.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) }))
	defer other.Close()
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-http":
			http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
		case "/to-other-host":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		case "/same-host":
			http.Redirect(w, r, server.URL+"/small", http.StatusFound)
		case "/small":
			_, _ = w.Write([]byte(`{"keys":[]}`))
		case "/huge":
			_, _ = w.Write([]byte(strings.Repeat("x", maxIssuerResponse+10)))
		}
	}))
	defer server.Close()
	client := newClient(server.Client().Transport)
	for _, test := range []struct {
		path string
		ok   bool
	}{
		{"/to-http", false},
		{"/to-other-host", false},
		{"/same-host", true},
		{"/huge", false},
	} {
		response, err := client.Get(server.URL + test.path)
		if err == nil {
			_, err = io.ReadAll(response.Body)
			_ = response.Body.Close()
		}
		if (err == nil) != test.ok {
			t.Errorf("%s: %v", test.path, err)
		}
	}
}

func TestVerifierTellsBadTokensFromUnreachableKeys(t *testing.T) {
	issuer := newFakeIssuer(t)
	store := &countingRules{rules: []overload.WorkloadRule{{Name: "k8s", Issuer: issuer.URL, Audience: audience, Conditions: map[string]string{"sub": "system:serviceaccount:ci:*"}, Scopes: []string{overload.ScopeModels}, Enabled: true}}}
	verifier, c := newTestVerifier(store, issuer.transport)
	ctx := context.Background()
	subject := "system:serviceaccount:ci:runner"

	wrongAlgorithm := oidctest.SignIDToken(issuer.key, "k", "RS384", fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":%q,"exp":%d}`, issuer.URL, audience, subject, c.Now().Add(time.Hour).Unix()))
	if _, err := verifier.Verify(ctx, wrongAlgorithm); !errors.Is(err, ErrRejected) {
		t.Fatalf("a first token with an algorithm the issuer does not use, before any keys were fetched: %v", err)
	}
	if issuer.keys.Load() != 0 {
		t.Fatal("a token with the wrong algorithm fetched keys")
	}
	if _, err := verifier.Verify(ctx, issuer.token(audience, subject, "", c.Now(), -time.Minute)); !errors.Is(err, ErrRejected) {
		t.Fatalf("an expired token: %v", err)
	}
	verifier.mu.Lock()
	clear(verifier.issuers)
	verifier.mu.Unlock()
	issuer.keysBroken.Store(true)
	if _, err := verifier.Verify(ctx, issuer.token(audience, subject, "", c.Now(), time.Hour)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable keys: %v", err)
	}
	if _, err := verifier.Verify(ctx, wrongAlgorithm); !errors.Is(err, ErrRejected) {
		t.Fatalf("a token with the wrong algorithm while the keys are unreachable: %v", err)
	}
	issuer.keysBroken.Store(false)
	c.advance(keyRefetchInterval)
	if _, err := verifier.Verify(ctx, issuer.token(audience, subject, "", c.Now(), time.Hour)); err != nil {
		t.Fatalf("after the keys came back: %v", err)
	}
}

func TestVerifierRediscoversDaily(t *testing.T) {
	issuer := newFakeIssuer(t)
	store := &countingRules{rules: []overload.WorkloadRule{{Name: "k8s", Issuer: issuer.URL, Audience: audience, Conditions: map[string]string{"sub": "system:serviceaccount:ci:*"}, Scopes: []string{overload.ScopeModels}, Enabled: true}}}
	verifier, c := newTestVerifier(store, issuer.transport)
	ctx := context.Background()
	token := func() string {
		return issuer.token(audience, "system:serviceaccount:ci:runner", "", c.Now(), time.Hour)
	}
	if _, err := verifier.Verify(ctx, token()); err != nil {
		t.Fatal(err)
	}
	issuer.broken.Store(true)
	c.advance(rediscoverEvery)
	if _, err := verifier.Verify(ctx, token()); err != nil {
		t.Fatalf("a token while the issuer is being read again: %v", err)
	}
	waitFor(t, func() bool { return issuer.discovery.Load() == 2 })
	waitFor(t, func() bool {
		verifier.mu.Lock()
		defer verifier.mu.Unlock()
		return !verifier.issuers[issuer.URL].refreshing
	})
	if _, err := verifier.Verify(ctx, token()); err != nil {
		t.Fatalf("a failed daily discovery dropped the working one: %v", err)
	}
	if issuer.discovery.Load() != 2 {
		t.Fatalf("a failed daily discovery was retried at once: %d", issuer.discovery.Load())
	}
	issuer.broken.Store(false)
	c.advance(discoveryRetry)
	if _, err := verifier.Verify(ctx, token()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return issuer.discovery.Load() == 3 })
	waitFor(t, func() bool {
		verifier.mu.Lock()
		defer verifier.mu.Unlock()
		return !verifier.issuers[issuer.URL].refreshing && verifier.issuers[issuer.URL].nextRefresh.After(c.Now())
	})
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !done(); {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUnverifiedIssuer(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://issuer.example"}`))
	for _, test := range []struct {
		name, raw, want string
	}{
		{"token", "e30." + payload + ".sig", "https://issuer.example"},
		{"too many parts", "e30." + payload + ".sig.extra", ""},
		{"too few parts", "e30." + payload, ""},
		{"oversized", "e30." + payload + "." + strings.Repeat(".", 20<<10), ""},
		{"no issuer", "e30.e30.sig", ""},
	} {
		got, err := unverifiedIssuer(test.raw)
		if got != test.want || (test.want == "") != errors.Is(err, ErrNotWorkloadToken) {
			t.Errorf("%s: %q, %v", test.name, got, err)
		}
	}
}
