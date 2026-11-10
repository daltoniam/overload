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
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/daltoniam/overload"
)

const (
	// ruleCacheTTL bounds how long a rule change takes to reach a replica
	// that did not make it; the replica that saves a rule forgets at once.
	ruleCacheTTL = 5 * time.Second
	// discoveryRetry is how long a failed discovery is remembered, so an
	// unreachable issuer is not asked again on every request.
	discoveryRetry = 30 * time.Second
	// keyRefetchInterval is the least time between fetches of an issuer's
	// keys. A token signed by an unknown key triggers a fetch, so without
	// this a stream of forged tokens would make overload hammer the issuer.
	keyRefetchInterval = time.Minute
	// rediscoverEvery is how often a working issuer's discovery document
	// is read again, in case its jwks_uri moved.
	rediscoverEvery = 24 * time.Hour
	fetchTimeout    = 10 * time.Second
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
	// Repository is the token's repository claim, if it has one (every
	// GitHub Actions token does).
	Repository string
	Claims     map[string]any
	Scopes     []string
}

// Label names the workload by the rules it matched and its repository,
// not its subject: a GitHub subject changes with every branch, pull request
// and environment, which would give one repository many identities in the
// fair queue and in usage.
func (identity Identity) Label() string {
	label := strings.Join(identity.Rules, ",")
	if identity.Repository != "" {
		label += " " + identity.Repository
	}
	return label
}

// Name identifies the workload in logs and queues.
func (identity Identity) Name() string { return "workload:" + identity.Label() }

// HasScope reports whether a matched rule grants scope.
func (identity Identity) HasScope(scope string) bool { return slices.Contains(identity.Scopes, scope) }

var (
	// ErrNotWorkloadToken means the credential is not a JWT, so other
	// authentication should be tried.
	ErrNotWorkloadToken = errors.New("not a workload token")
	// ErrRejected wraps the reason a token is refused. Its message is safe
	// to show the caller.
	ErrRejected = errors.New("workload token rejected")
	// ErrUnavailable means the token could not be checked because the rules
	// or the issuer could not be read. Details are logged, not returned, so
	// database and issuer errors never reach unauthenticated callers.
	ErrUnavailable = errors.New("unable to check the workload token; retry later")
)

// Verifier checks workload tokens against the rules.
type Verifier struct {
	rules  RuleStore
	client *http.Client
	now    func() time.Time

	rulesMu  sync.Mutex
	cached   []overload.WorkloadRule
	cachedAt time.Time
	loaded   bool

	mu      sync.Mutex
	issuers map[string]*issuer
}

// NewVerifier returns a verifier that fetches issuers' discovery documents
// and keys through transport (nil for the default).
func NewVerifier(rules RuleStore, transport http.RoundTripper) *Verifier {
	return &Verifier{rules: rules, client: newClient(transport), now: time.Now, issuers: map[string]*issuer{}}
}

// Forget drops the cached rules, so a saved or deleted rule applies to the
// next token.
func (verifier *Verifier) Forget() {
	verifier.rulesMu.Lock()
	defer verifier.rulesMu.Unlock()
	verifier.loaded = false
}

// Verify checks a token's signature, expiry and audience and returns the
// workload with the scopes of every enabled rule it matches. Errors wrap
// ErrNotWorkloadToken, ErrRejected or ErrUnavailable.
func (verifier *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	issuerURL, err := unverifiedIssuer(raw)
	if err != nil {
		return Identity{}, err
	}
	rules, err := verifier.enabledRules(ctx)
	if err != nil {
		slog.Warn("workload rules unavailable", "error", err)
		return Identity{}, ErrUnavailable
	}
	var candidates []overload.WorkloadRule
	for _, rule := range rules {
		if rule.Issuer == issuerURL {
			candidates = append(candidates, rule)
		}
	}
	if len(candidates) == 0 {
		return Identity{}, fmt.Errorf("%w: no workload rule trusts its issuer", ErrRejected)
	}
	entry, err := verifier.issuer(ctx, issuerURL)
	if err != nil {
		return Identity{}, ErrUnavailable
	}
	checkCtx, keys := withKeyCheck(ctx)
	token, err := entry.verifier.Verify(checkCtx, raw)
	if err != nil {
		// Malformed tokens and unadvertised algorithms fail before the keys
		// are asked, and expired or not-yet-valid ones after the signature
		// checks out, so only a failed key fetch is overload's problem.
		if keys.unavailable {
			return Identity{}, ErrUnavailable
		}
		return Identity{}, fmt.Errorf("%w: it is invalid or expired", ErrRejected)
	}
	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: its claims are unreadable", ErrRejected)
	}
	identity := Identity{Issuer: token.Issuer, Subject: token.Subject, Claims: claims}
	identity.Repository, _ = claims["repository"].(string)
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
		return Identity{}, fmt.Errorf("%w: no workload rule matches its audience and claims", ErrRejected)
	}
	return identity, nil
}

// enabledRules returns the enabled rules, read from the store at most every
// ruleCacheTTL so tokens do not each cost a query.
func (verifier *Verifier) enabledRules(ctx context.Context) ([]overload.WorkloadRule, error) {
	verifier.rulesMu.Lock()
	defer verifier.rulesMu.Unlock()
	if verifier.loaded && verifier.now().Sub(verifier.cachedAt) < ruleCacheTTL {
		return verifier.cached, nil
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	rules, err := verifier.rules.ListWorkloadRules(ctx)
	if err != nil {
		return nil, err
	}
	enabled := make([]overload.WorkloadRule, 0, len(rules))
	issuers := map[string]bool{}
	for _, rule := range rules {
		if rule.Enabled {
			enabled = append(enabled, rule)
			issuers[rule.Issuer] = true
		}
	}
	verifier.cached, verifier.cachedAt, verifier.loaded = enabled, verifier.now(), true
	verifier.mu.Lock()
	for name := range verifier.issuers {
		if !issuers[name] {
			delete(verifier.issuers, name)
		}
	}
	verifier.mu.Unlock()
	return enabled, nil
}

// issuer is one token issuer's discovery result and keys.
type issuer struct {
	// ready closes when discovery finishes; the fields below are set
	// before then and never change after.
	ready      chan struct{}
	verifier   *oidc.IDTokenVerifier
	keys       *keySet
	err        error
	finishedAt time.Time

	// nextRefresh is when a successful discovery is read again, so a
	// changed jwks_uri is picked up; refreshing marks one in progress.
	// Both are guarded by Verifier.mu.
	nextRefresh time.Time
	refreshing  bool
}

// refreshDue reports whether a successful discovery should be read again.
// The caller holds Verifier.mu.
func (entry *issuer) refreshDue(now time.Time) bool {
	select {
	case <-entry.ready:
		return entry.err == nil && !entry.refreshing && !now.Before(entry.nextRefresh)
	default:
		return false
	}
}

// retryDue reports whether a finished discovery failed long enough ago to
// try again.
func (entry *issuer) retryDue(now time.Time) bool {
	select {
	case <-entry.ready:
		return entry.err != nil && now.Sub(entry.finishedAt) >= discoveryRetry
	default:
		return false
	}
}

// issuer returns the issuer's verifier, discovering it once for all the
// requests that need it. Discovery runs without the map lock held, so a
// slow issuer only delays tokens from that issuer.
func (verifier *Verifier) issuer(ctx context.Context, issuerURL string) (*issuer, error) {
	verifier.mu.Lock()
	entry := verifier.issuers[issuerURL]
	now := verifier.now()
	switch {
	case entry == nil || entry.retryDue(now):
		entry = &issuer{ready: make(chan struct{}), nextRefresh: now.Add(rediscoverEvery)}
		verifier.issuers[issuerURL] = entry
		go verifier.discover(entry, issuerURL)
	case entry.refreshDue(now):
		entry.refreshing = true
		go verifier.rediscover(entry, issuerURL)
	}
	verifier.mu.Unlock()
	select {
	case <-entry.ready:
		return entry, entry.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// discover reads the issuer's discovery document. It runs on its own
// context so a caller that gives up does not fail it for the others.
func (verifier *Verifier) discover(entry *issuer, issuerURL string) {
	defer close(entry.ready)
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	entry.verifier, entry.keys, entry.err = verifier.discoverNow(ctx, issuerURL)
	entry.finishedAt = verifier.now()
	if entry.err != nil {
		slog.Warn("workload token issuer unavailable", "issuer", issuerURL, "error", entry.err)
	}
}

// rediscover reads a working issuer's discovery document again while
// tokens keep being checked with the old one, which stays in use if the
// issuer cannot be read right now.
func (verifier *Verifier) rediscover(old *issuer, issuerURL string) {
	fresh := &issuer{ready: make(chan struct{})}
	verifier.discover(fresh, issuerURL)
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	now := verifier.now()
	if fresh.err != nil {
		old.refreshing, old.nextRefresh = false, now.Add(discoveryRetry)
		return
	}
	fresh.nextRefresh = now.Add(rediscoverEvery)
	if verifier.issuers[issuerURL] == old {
		verifier.issuers[issuerURL] = fresh
	}
}

func (verifier *Verifier) discoverNow(ctx context.Context, issuerURL string) (*oidc.IDTokenVerifier, *keySet, error) {
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, verifier.client), issuerURL)
	if err != nil {
		return nil, nil, err
	}
	var metadata struct {
		JWKSURL    string   `json:"jwks_uri"`
		Algorithms []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, nil, err
	}
	if parsed, err := url.Parse(metadata.JWKSURL); err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, nil, fmt.Errorf("the discovery document's jwks_uri %q is not an https URL", metadata.JWKSURL)
	}
	keys := &keySet{url: metadata.JWKSURL, client: verifier.client, now: verifier.now}
	config := &oidc.Config{SkipClientIDCheck: true, SupportedSigningAlgs: signingAlgorithms(metadata.Algorithms), Now: verifier.now}
	return oidc.NewVerifier(issuerURL, keys, config), keys, nil
}

// signingAlgorithms keeps the asymmetric algorithms go-oidc verifies; with
// none, go-oidc expects RS256.
func signingAlgorithms(advertised []string) []string {
	var algorithms []string
	for _, algorithm := range advertised {
		switch algorithm {
		case oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512, oidc.PS256, oidc.PS384, oidc.PS512, oidc.EdDSA:
			algorithms = append(algorithms, algorithm)
		}
	}
	return algorithms
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
