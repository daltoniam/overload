package httpapi

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// accessHeader carries the signed identity Cloudflare Access adds to every
// request it lets through to the origin.
const accessHeader = "Cf-Access-Jwt-Assertion"

// accessVerifier accepts a request only with a valid Cloudflare Access token
// for one application, so the dashboard cannot be reached by going around
// Access (for example from inside the cluster).
type accessVerifier struct {
	issuer   string
	audience string
	client   *http.Client
	now      func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// accessFromEnv returns nil when Cloudflare Access is not configured.
func accessFromEnv() (*accessVerifier, error) {
	team, audience := strings.TrimSpace(os.Getenv("OVERLOAD_ACCESS_TEAM_DOMAIN")), strings.TrimSpace(os.Getenv("OVERLOAD_ACCESS_AUD"))
	if team == "" && audience == "" {
		return nil, nil
	}
	if team == "" || audience == "" {
		return nil, errors.New("set both OVERLOAD_ACCESS_TEAM_DOMAIN and OVERLOAD_ACCESS_AUD for Cloudflare Access")
	}
	return newAccessVerifier(team, audience)
}

func newAccessVerifier(team, audience string) (*accessVerifier, error) {
	if !strings.Contains(team, "://") {
		team = "https://" + team
	}
	parsed, err := url.Parse(team)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || strings.Trim(parsed.Path, "/") != "" || parsed.RawQuery != "" || parsed.User != nil {
		return nil, errors.New("OVERLOAD_ACCESS_TEAM_DOMAIN must be a host such as myteam.cloudflareaccess.com")
	}
	return &accessVerifier{
		issuer:   "https://" + strings.ToLower(parsed.Host),
		audience: audience,
		client:   &http.Client{Timeout: 5 * time.Second},
		now:      time.Now,
	}, nil
}

type accessClaims struct {
	Issuer    string          `json:"iss"`
	Audience  json.RawMessage `json:"aud"`
	Email     string          `json:"email"`
	Expires   int64           `json:"exp"`
	NotBefore int64           `json:"nbf"`
}

// verify returns the signed-in email (empty for service tokens).
func (v *accessVerifier) verify(ctx context.Context, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return "", err
	}
	if header.Algorithm != "RS256" || header.KeyID == "" {
		return "", errors.New("unsupported token algorithm")
	}
	key, err := v.key(ctx, header.KeyID)
	if err != nil {
		return "", err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("malformed signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return "", errors.New("invalid signature")
	}
	var claims accessClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return "", err
	}
	if claims.Issuer != v.issuer {
		return "", errors.New("wrong issuer")
	}
	if !audienceIncludes(claims.Audience, v.audience) {
		return "", errors.New("wrong audience")
	}
	now := v.now().Unix()
	const skew = 60
	if claims.Expires == 0 || now > claims.Expires+skew {
		return "", errors.New("token expired")
	}
	if claims.NotBefore != 0 && now+skew < claims.NotBefore {
		return "", errors.New("token not yet valid")
	}
	return claims.Email, nil
}

func decodeSegment(segment string, target any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return errors.New("malformed token")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return errors.New("malformed token")
	}
	return nil
}

func audienceIncludes(raw json.RawMessage, audience string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == audience
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	for _, value := range many {
		if value == audience {
			return true
		}
	}
	return false
}

// key returns the signing key, refetching the team's keys hourly and when
// an unknown key appears (Access rotates keys), at most once a minute.
func (v *accessVerifier) key(ctx context.Context, id string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	age := v.now().Sub(v.fetched)
	key, ok := v.keys[id]
	if ok && age < time.Hour {
		return key, nil
	}
	if v.keys == nil || age >= time.Minute {
		keys, err := v.fetchKeys(ctx)
		if err != nil {
			if ok {
				return key, nil
			}
			return nil, err
		}
		v.keys, v.fetched = keys, v.now()
		key, ok = keys[id]
	}
	if !ok {
		return nil, errors.New("unknown signing key")
	}
	return key, nil
}

func (v *accessVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.issuer+"/cdn-cgi/access/certs", nil)
	if err != nil {
		return nil, err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch Cloudflare Access keys: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch Cloudflare Access keys: status %d", response.StatusCode)
	}
	var body struct {
		Keys []struct {
			KeyID   string `json:"kid"`
			Type    string `json:"kty"`
			Modulus string `json:"n"`
			Expo    string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body); err != nil {
		return nil, errors.New("invalid Cloudflare Access keys")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jwk := range body.Keys {
		if jwk.Type != "RSA" || jwk.KeyID == "" {
			continue
		}
		modulus, err := base64.RawURLEncoding.DecodeString(jwk.Modulus)
		if err != nil || len(modulus) < 256 {
			continue
		}
		exponent, err := base64.RawURLEncoding.DecodeString(jwk.Expo)
		if err != nil || len(exponent) == 0 || len(exponent) > 4 {
			continue
		}
		keys[jwk.KeyID] = &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(new(big.Int).SetBytes(exponent).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("no Cloudflare Access keys")
	}
	return keys, nil
}
