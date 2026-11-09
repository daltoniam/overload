package workload

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/daltoniam/overload"
)

type rules []overload.WorkloadRule

func (list rules) ListWorkloadRules(context.Context) ([]overload.WorkloadRule, error) {
	return list, nil
}

// Issuer is a fake OIDC issuer for workload tokens.
type Issuer struct {
	URL      string
	Client   *http.Client
	key      *rsa.PrivateKey
	requests atomic.Int64
}

// NewIssuer starts a fake issuer for the test.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &Issuer{key: key}
	discovery := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "k", Algorithm: "RS256"}}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer.requests.Add(1)
		discovery.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	discovery.SetIssuer(server.URL)
	issuer.URL, issuer.Client = server.URL, server.Client()
	return issuer
}

// Token signs a token with extra claims.
func (issuer *Issuer) Token(audience, subject string, claims string, lifetime time.Duration) string {
	body := fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":%q,"iat":%d,"exp":%d%s}`, issuer.URL, audience, subject, time.Now().Unix(), time.Now().Add(lifetime).Unix(), claims)
	return oidctest.SignIDToken(issuer.key, "k", "RS256", body)
}

func TestVerify(t *testing.T) {
	issuer := NewIssuer(t)
	trusted := rules{
		{Name: "ci", Issuer: issuer.URL, Audience: "https://overload.example.com", Conditions: map[string]string{"repository": "acme/*", "ref": "refs/heads/main"}, Scopes: []string{overload.ScopeReviews}, Enabled: true},
		{Name: "models", Issuer: issuer.URL, Audience: "https://overload.example.com", Conditions: map[string]string{"repository": "acme/agents"}, Scopes: []string{overload.ScopeModels}, Enabled: true},
		{Name: "off", Issuer: issuer.URL, Audience: "https://overload.example.com", Conditions: map[string]string{"repository": "*/*"}, Scopes: []string{overload.ScopeModels}, Enabled: false},
	}
	verifier := NewVerifier(trusted, issuer.Client)
	ctx := context.Background()

	identity, err := verifier.Verify(ctx, issuer.Token("https://overload.example.com", "repo:acme/agents:ref:refs/heads/main", `,"repository":"acme/agents","ref":"refs/heads/main"`, time.Minute))
	if err != nil || !identity.HasScope(overload.ScopeReviews) || !identity.HasScope(overload.ScopeModels) || len(identity.Rules) != 2 || !strings.HasPrefix(identity.Name(), "workload:ci,models:") {
		t.Fatalf("identity = %+v, %v", identity, err)
	}
	for name, token := range map[string]string{
		"wrong branch":   issuer.Token("https://overload.example.com", "s", `,"repository":"acme/web","ref":"refs/heads/feature"`, time.Minute),
		"wrong audience": issuer.Token("https://elsewhere.example.com", "s", `,"repository":"acme/web","ref":"refs/heads/main"`, time.Minute),
		"expired":        issuer.Token("https://overload.example.com", "s", `,"repository":"acme/web","ref":"refs/heads/main"`, -time.Minute),
		"non-string":     issuer.Token("https://overload.example.com", "s", `,"repository":["acme/web"],"ref":"refs/heads/main"`, time.Minute),
		"forged":         issuer.Token("https://overload.example.com", "s", `,"repository":"acme/web","ref":"refs/heads/main"`, time.Minute)[:40] + "x.y.z",
	} {
		if _, err := verifier.Verify(ctx, token); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := verifier.Verify(ctx, "ovl_key"); !errors.Is(err, ErrNotWorkloadToken) {
		t.Fatalf("API key: %v", err)
	}
	before := issuer.requests.Load()
	other := NewIssuer(t)
	if _, err := verifier.Verify(ctx, other.Token("https://overload.example.com", "s", `,"repository":"acme/web"`, time.Minute)); err == nil || !strings.Contains(err.Error(), "no workload rule trusts") {
		t.Fatalf("untrusted issuer: %v", err)
	}
	if other.requests.Load() != 0 || issuer.requests.Load() != before {
		t.Fatal("an untrusted issuer was contacted")
	}
}
