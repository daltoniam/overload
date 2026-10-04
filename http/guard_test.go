package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("OVERLOAD_BASE_URL", "http://example.com")
	os.Exit(m.Run())
}

func TestUnknownHostsAreRejected(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	handler := Handler(&fakeStore{})
	for _, test := range []struct {
		host string
		want int
	}{
		{"127.0.0.1:8082", http.StatusOK},
		{"localhost:8082", http.StatusOK},
		{"[::1]:8082", http.StatusOK},
		{"example.com", http.StatusOK},
		{"attacker.test:8082", http.StatusMisdirectedRequest},
	} {
		request := httptest.NewRequest(http.MethodGet, "/settings", nil)
		request.Host = test.host
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("host %s: %d, want %d", test.host, response.Code, test.want)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{}"))
	request.Host = "hooks.example.net"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code == http.StatusMisdirectedRequest {
		t.Fatal("webhook endpoint must accept the tunnel's public host")
	}
}

func TestCrossSiteStateChangesAreRejected(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	handler := Handler(&fakeStore{})
	for _, test := range []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{"cross-site fetch", "/settings/legacy/delete", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"foreign origin", "/settings/legacy/delete", map[string]string{"Origin": "http://attacker.test"}, http.StatusForbidden},
		{"same origin without token", "/settings/legacy/delete", map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}, http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(url.Values{}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for key, value := range test.headers {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want || (test.name != "same origin without token" && !strings.Contains(response.Body.String(), "origin")) {
			t.Errorf("%s: %d %q", test.name, response.Code, response.Body.String())
		}
	}
}

func TestOneTimeTokens(t *testing.T) {
	tokens := newOneTimeTokens(time.Minute)
	token := tokens.issue()
	if !tokens.consume(token) || tokens.consume(token) || tokens.consume("") {
		t.Fatal("token must be accepted exactly once")
	}
	expired := newOneTimeTokens(-time.Second)
	if expired.consume(expired.issue()) {
		t.Fatal("expired token accepted")
	}
}
