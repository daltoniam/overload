package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/daltoniam/overload/tunnel"
)

type fakeTunnel struct {
	status   tunnel.Status
	setups   []string
	logins   int
	stopped  bool
	setupErr error
}

func (fake *fakeTunnel) Status() tunnel.Status { return fake.status }
func (fake *fakeTunnel) Login() error          { fake.logins++; return nil }
func (fake *fakeTunnel) Setup(name, hostname string) error {
	fake.setups = append(fake.setups, name+" "+hostname)
	return fake.setupErr
}
func (fake *fakeTunnel) Stop() error { fake.stopped = true; return nil }

type tunnelStore struct {
	fakeStore
	tunnel *fakeTunnel
}

func (store *tunnelStore) Tunnel() TunnelControl { return store.tunnel }

func TestTunnelSetupPage(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	fake := &fakeTunnel{status: tunnel.Status{Supported: true, Cloudflared: "/opt/homebrew/bin/cloudflared"}}
	handler := Handler(&tunnelStore{tunnel: fake})
	get := func(path string) string {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, response.Code)
		}
		return response.Body.String()
	}
	page := get("/setup/tunnel")
	if !strings.Contains(page, "Log in to Cloudflare") || strings.Contains(page, `name="hostname"`) {
		t.Fatal("before login the page must offer login, not setup")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page)[1]
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", csrf)
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := post("/setup/tunnel/login", url.Values{}); response.Code != http.StatusSeeOther || fake.logins != 1 {
		t.Fatalf("login: %d %d", response.Code, fake.logins)
	}
	fake.status.LoggedIn = true
	if page := get("/setup/tunnel"); !strings.Contains(page, `name="hostname"`) || !strings.Contains(page, `value="overload"`) {
		t.Fatal("after login the page must offer setup")
	}
	if response := post("/setup/tunnel", url.Values{"name": {"overload"}, "hostname": {"hooks.example.com"}}); response.Code != http.StatusSeeOther || fake.setups[0] != "overload hooks.example.com" {
		t.Fatalf("setup: %d %v", response.Code, fake.setups)
	}
	fake.status.Job = tunnel.Job{Kind: "setup", Running: true, Steps: []string{"Created tunnel overload."}}
	if fragment := get("/setup/tunnel/status"); !strings.Contains(fragment, `hx-get="/setup/tunnel/status"`) || !strings.Contains(fragment, "Created tunnel overload.") {
		t.Fatalf("running job must poll: %s", fragment)
	}
	fake.status.Job = tunnel.Job{Kind: "setup", Err: "cloudflared tunnel route dns failed: record exists"}
	fake.status.Config = tunnel.Config{Name: "overload", ID: "id", Hostname: "hooks.example.com"}
	page = get("/setup/tunnel")
	if strings.Contains(page, `hx-get="/setup/tunnel/status"`) || !strings.Contains(page, "record exists") || !strings.Contains(page, "https://hooks.example.com/webhooks/github") {
		t.Fatal("finished job must stop polling and show the error and webhook URL")
	}
	if response := post("/setup/tunnel/stop", url.Values{}); response.Code != http.StatusSeeOther || !fake.stopped {
		t.Fatal("stop")
	}
	request := httptest.NewRequest(http.MethodPost, "/setup/tunnel", strings.NewReader("name=x&hostname=y"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("setup without CSRF token: %d", response.Code)
	}
}

func TestTunnelPageOffMacOS(t *testing.T) {
	t.Setenv("OVERLOAD_UI_INSECURE", "1")
	handler := Handler(&tunnelStore{tunnel: &fakeTunnel{}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/setup/tunnel", nil))
	if !strings.Contains(response.Body.String(), "docs/cloudflare-tunnel.md") || strings.Contains(response.Body.String(), "Log in to Cloudflare") {
		t.Fatal("off macOS the page must point to the guide")
	}
}
