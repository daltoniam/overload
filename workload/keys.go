package workload

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
)

// maxIssuerResponse caps discovery documents and key sets; real ones are a
// few kilobytes.
const maxIssuerResponse = 1 << 20

// newClient returns the client that reads issuers' documents: bounded in
// time and size, and following redirects only to https on the same host,
// so a rule's issuer cannot bounce overload to plain HTTP or to another
// (possibly internal) host.
func newClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: limitedTransport{next: transport, limit: maxIssuerResponse},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if request.URL.Scheme != "https" || request.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refused redirect to %s://%s", request.URL.Scheme, request.URL.Host)
			}
			return nil
		},
	}
}

type limitedTransport struct {
	next  http.RoundTripper
	limit int64
}

func (transport limitedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &limitedBody{ReadCloser: response.Body, remaining: transport.limit}
	return response, nil
}

var errResponseTooLarge = errors.New("issuer response too large")

// limitedBody fails a read past its limit instead of truncating quietly,
// so an oversized document is an error rather than unparseable JSON.
type limitedBody struct {
	io.ReadCloser
	remaining int64
}

func (body *limitedBody) Read(buffer []byte) (int, error) {
	if int64(len(buffer)) > body.remaining+1 {
		buffer = buffer[:body.remaining+1]
	}
	n, err := body.ReadCloser.Read(buffer)
	if int64(n) > body.remaining {
		n = int(body.remaining)
		body.remaining = 0
		return n, errResponseTooLarge
	}
	body.remaining -= int64(n)
	return n, err
}

// keySet verifies signatures with an issuer's published keys, fetching
// them again when a token is signed by a key it does not know, but at most
// once per keyRefetchInterval. go-oidc's remote key set fetches on every
// unknown key.
type keySet struct {
	url    string
	client *http.Client
	now    func() time.Time

	// fetchMu lets one request fetch while others wait for its result.
	fetchMu sync.Mutex

	mu         sync.Mutex
	keys       oidc.StaticKeySet
	generation int
	fetchedAt  time.Time
}

func (keys *keySet) snapshot() (oidc.StaticKeySet, int, time.Time) {
	keys.mu.Lock()
	defer keys.mu.Unlock()
	return keys.keys, keys.generation, keys.fetchedAt
}

// loaded reports whether the keys were ever fetched.
func (keys *keySet) loaded() bool {
	current, _, _ := keys.snapshot()
	return len(current.PublicKeys) > 0
}

func (keys *keySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	current, generation, _ := keys.snapshot()
	if len(current.PublicKeys) > 0 {
		if payload, err := current.VerifySignature(ctx, jwt); err == nil {
			return payload, nil
		}
	}
	if !keys.refresh(ctx, generation) {
		return nil, errors.New("no known key verifies the token")
	}
	current, _, _ = keys.snapshot()
	return current.VerifySignature(ctx, jwt)
}

// refresh fetches the keys unless another request already did since
// generation seen, or the last fetch was too recent. It reports whether
// newer keys are available.
func (keys *keySet) refresh(ctx context.Context, seen int) bool {
	keys.fetchMu.Lock()
	defer keys.fetchMu.Unlock()
	_, generation, fetchedAt := keys.snapshot()
	if generation != seen {
		return true
	}
	if !fetchedAt.IsZero() && keys.now().Sub(fetchedAt) < keyRefetchInterval {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	fetched, err := keys.fetch(ctx)
	keys.mu.Lock()
	defer keys.mu.Unlock()
	keys.fetchedAt = keys.now()
	if err != nil {
		slog.Warn("workload token issuer keys unavailable", "url", keys.url, "error", err)
		return false
	}
	keys.keys = oidc.StaticKeySet{PublicKeys: fetched}
	keys.generation++
	return true
}

func (keys *keySet) fetch(ctx context.Context) ([]crypto.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, keys.url, nil)
	if err != nil {
		return nil, err
	}
	response, err := keys.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key set answered %s", response.Status)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(response.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("read key set: %w", err)
	}
	var public []crypto.PublicKey
	for _, key := range set.Keys {
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		switch key.Key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
			public = append(public, key.Key)
		}
	}
	if len(public) == 0 {
		return nil, errors.New("the key set has no signing keys")
	}
	return public, nil
}
