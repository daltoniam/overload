// Package workload accepts OIDC tokens from automation (GitHub Actions jobs,
// Kubernetes service accounts) in place of stored secrets. Administrators
// write rules naming the issuer, audience and claims to trust; a token is
// accepted only from an issuer some enabled rule names.
package workload

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/daltoniam/overload"
)

// RuleStore lists the workload rules.
type RuleStore interface {
	ListWorkloadRules(context.Context) ([]overload.WorkloadRule, error)
}

// Identity is a verified workload.
type Identity struct {
	// Rules are the rules that matched.
	Rules   []string
	Issuer  string
	Subject string
	Claims  map[string]any
	Scopes  []string
}

// Name identifies the workload in logs and queues.
func (identity Identity) Name() string {
	return "workload:" + strings.Join(identity.Rules, ",") + ":" + identity.Subject
}

// HasScope reports whether a matched rule grants scope.
func (identity Identity) HasScope(scope string) bool { return slices.Contains(identity.Scopes, scope) }

// ErrNotWorkloadToken means the credential is not a JWT, so other
// authentication should be tried.
var ErrNotWorkloadToken = errors.New("not a workload token")

// Verifier checks workload tokens against the rules.
type Verifier struct {
	rules  RuleStore
	client *http.Client

	mu        sync.Mutex
	providers map[string]*oidc.Provider
}

// NewVerifier returns a verifier; client fetches issuers' discovery
// documents and keys.
func NewVerifier(rules RuleStore, client *http.Client) *Verifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Verifier{rules: rules, client: client, providers: map[string]*oidc.Provider{}}
}

// Verify checks a token's signature, expiry and audience and returns the
// workload with the scopes of every enabled rule it matches.
func (verifier *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	issuer, err := unverifiedIssuer(raw)
	if err != nil {
		return Identity{}, err
	}
	rules, err := verifier.rules.ListWorkloadRules(ctx)
	if err != nil {
		return Identity{}, err
	}
	var candidates []overload.WorkloadRule
	for _, rule := range rules {
		if rule.Enabled && rule.Issuer == issuer {
			candidates = append(candidates, rule)
		}
	}
	if len(candidates) == 0 {
		return Identity{}, errors.New("no workload rule trusts this token's issuer")
	}
	provider, err := verifier.provider(ctx, issuer)
	if err != nil {
		return Identity{}, fmt.Errorf("reach the token issuer: %w", err)
	}
	token, err := provider.Verifier(&oidc.Config{SkipClientIDCheck: true}).Verify(oidc.ClientContext(ctx, verifier.client), raw)
	if err != nil {
		return Identity{}, errors.New("the workload token is invalid or expired")
	}
	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		return Identity{}, errors.New("the workload token has unreadable claims")
	}
	identity := Identity{Issuer: token.Issuer, Subject: token.Subject, Claims: claims}
	for _, rule := range candidates {
		if !slices.Contains(token.Audience, rule.Audience) || !rule.Matches(claims) {
			continue
		}
		identity.Rules = append(identity.Rules, rule.Name)
		for _, scope := range rule.Scopes {
			if !identity.HasScope(scope) {
				identity.Scopes = append(identity.Scopes, scope)
			}
		}
	}
	if len(identity.Rules) == 0 {
		return Identity{}, errors.New("no workload rule matches this token's audience and claims")
	}
	return identity, nil
}

func (verifier *Verifier) provider(ctx context.Context, issuer string) (*oidc.Provider, error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if provider, ok := verifier.providers[issuer]; ok {
		return provider, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, verifier.client), issuer)
	if err != nil {
		return nil, err
	}
	verifier.providers[issuer] = provider
	return provider, nil
}

// unverifiedIssuer reads a JWT's iss claim to pick the rules to check it
// against; nothing is trusted until the signature is verified.
func unverifiedIssuer(raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || len(raw) > 16<<10 {
		return "", ErrNotWorkloadToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ErrNotWorkloadToken
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Issuer == "" {
		return "", ErrNotWorkloadToken
	}
	return claims.Issuer, nil
}
