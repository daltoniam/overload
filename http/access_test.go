package httpapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type accessFixture struct {
	key     *rsa.PrivateKey
	kid     string
	server  *httptest.Server
	fetches atomic.Int32
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &accessFixture{key: key, kid: "key-1"}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cdn-cgi/access/certs" {
			http.NotFound(w, r)
			return
		}
		fixture.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": fixture.kid, "kty": "RSA", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(fixture.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(fixture.key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *accessFixture) verifier(t *testing.T, now time.Time) *accessVerifier {
	t.Helper()
	verifier, err := newAccessVerifier(fixture.server.URL, "app-aud")
	if err != nil {
		t.Fatal(err)
	}
	verifier.client = fixture.server.Client()
	verifier.now = func() time.Time { return now }
	return verifier
}

func (fixture *accessFixture) token(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signed := encode(header) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, fixture.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestAccessVerifier(t *testing.T) {
	fixture := newAccessFixture(t)
	now := time.Unix(1_800_000_000, 0)
	issuer := strings.ToLower(fixture.server.URL)
	valid := func() map[string]any {
		return map[string]any{"iss": issuer, "aud": []string{"app-aud"}, "email": "dev@example.com", "exp": now.Add(time.Hour).Unix(), "nbf": now.Add(-time.Minute).Unix()}
	}
	header := map[string]any{"alg": "RS256", "kid": fixture.kid}
	verifier := fixture.verifier(t, now)

	email, err := verifier.verify(context.Background(), fixture.token(t, header, valid()))
	if err != nil || email != "dev@example.com" {
		t.Fatalf("valid token: email=%q err=%v", email, err)
	}
	single := valid()
	single["aud"] = "app-aud"
	if _, err := verifier.verify(context.Background(), fixture.token(t, header, single)); err != nil {
		t.Fatalf("string audience: %v", err)
	}

	cases := map[string]func() string{
		"empty":     func() string { return "" },
		"wrong aud": func() string { c := valid(); c["aud"] = []string{"other"}; return fixture.token(t, header, c) },
		"wrong issuer": func() string {
			c := valid()
			c["iss"] = "https://evil.cloudflareaccess.com"
			return fixture.token(t, header, c)
		},
		"expired": func() string {
			c := valid()
			c["exp"] = now.Add(-2 * time.Minute).Unix()
			return fixture.token(t, header, c)
		},
		"no expiry": func() string { c := valid(); delete(c, "exp"); return fixture.token(t, header, c) },
		"not yet": func() string {
			c := valid()
			c["nbf"] = now.Add(5 * time.Minute).Unix()
			return fixture.token(t, header, c)
		},
		"alg none": func() string {
			parts := strings.Split(fixture.token(t, map[string]any{"alg": "none", "kid": fixture.kid}, valid()), ".")
			return parts[0] + "." + parts[1] + "."
		},
		"tampered": func() string {
			parts := strings.Split(fixture.token(t, header, valid()), ".")
			other := valid()
			other["email"] = "attacker@example.com"
			raw, _ := json.Marshal(other)
			return parts[0] + "." + base64.RawURLEncoding.EncodeToString(raw) + "." + parts[2]
		},
		"unknown key": func() string { return fixture.token(t, map[string]any{"alg": "RS256", "kid": "key-2"}, valid()) },
	}
	for name, token := range cases {
		if _, err := verifier.verify(context.Background(), token()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if fetches := fixture.fetches.Load(); fetches != 1 {
		t.Fatalf("keys fetched %d times, want 1 (unknown keys refetch at most once a minute)", fetches)
	}
}

func TestAccessVerifierRefetchesRotatedKeys(t *testing.T) {
	fixture := newAccessFixture(t)
	now := time.Unix(1_800_000_000, 0)
	verifier := fixture.verifier(t, now)
	claims := map[string]any{"iss": strings.ToLower(fixture.server.URL), "aud": "app-aud", "exp": now.Add(time.Hour).Unix()}
	if _, err := verifier.verify(context.Background(), fixture.token(t, map[string]any{"alg": "RS256", "kid": "key-1"}, claims)); err != nil {
		t.Fatal(err)
	}
	fixture.kid = "key-2"
	now = now.Add(2 * time.Minute)
	verifier.now = func() time.Time { return now }
	if _, err := verifier.verify(context.Background(), fixture.token(t, map[string]any{"alg": "RS256", "kid": "key-2"}, claims)); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
}

func TestAccessFromEnv(t *testing.T) {
	t.Setenv("OVERLOAD_ACCESS_TEAM_DOMAIN", "")
	t.Setenv("OVERLOAD_ACCESS_AUD", "")
	if verifier, err := accessFromEnv(); verifier != nil || err != nil {
		t.Fatalf("unset: %v %v", verifier, err)
	}
	t.Setenv("OVERLOAD_ACCESS_TEAM_DOMAIN", "myteam.cloudflareaccess.com")
	if _, err := accessFromEnv(); err == nil {
		t.Fatal("team domain without audience accepted")
	}
	t.Setenv("OVERLOAD_ACCESS_AUD", "aud")
	verifier, err := accessFromEnv()
	if err != nil || verifier.issuer != "https://myteam.cloudflareaccess.com" {
		t.Fatalf("verifier %+v err %v", verifier, err)
	}
	for _, bad := range []string{"http://myteam.cloudflareaccess.com", "https://myteam.cloudflareaccess.com/path", "https://user@myteam.cloudflareaccess.com"} {
		t.Setenv("OVERLOAD_ACCESS_TEAM_DOMAIN", bad)
		if _, err := accessFromEnv(); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestSecureRequiresAccessToken(t *testing.T) {
	fixture := newAccessFixture(t)
	t.Setenv("OVERLOAD_ACCESS_TEAM_DOMAIN", fixture.server.URL)
	t.Setenv("OVERLOAD_ACCESS_AUD", "app-aud")
	t.Setenv("OVERLOAD_UI_USER", "")
	t.Setenv("OVERLOAD_UI_PASSWORD", "")
	t.Setenv("OVERLOAD_UI_INSECURE", "")
	t.Setenv("OVERLOAD_BASE_URL", "https://overload.example.com")
	if err := ValidateAuth(); err != nil {
		t.Fatalf("Access should replace the UI password: %v", err)
	}
	handler := secure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	verifierClient := fixture.server.Client()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = verifierClient.Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	request := httptest.NewRequest(http.MethodGet, "https://overload.example.com/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("no token: %d", recorder.Code)
	}

	claims := map[string]any{"iss": strings.ToLower(fixture.server.URL), "aud": "app-aud", "exp": time.Now().Add(time.Hour).Unix()}
	request = httptest.NewRequest(http.MethodGet, "https://overload.example.com/", nil)
	request.Header.Set(accessHeader, fixture.token(t, map[string]any{"alg": "RS256", "kid": fixture.kid}, claims))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("valid token: %d %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "https://overload.example.com/webhooks/github", nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("webhook must not need Access: %d", recorder.Code)
	}
}
