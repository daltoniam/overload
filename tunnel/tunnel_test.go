package tunnel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daltoniam/overload/launchd"
)

type fakeService struct {
	mu        sync.Mutex
	installed []launchd.Agent
	removed   []string
}

func (service *fakeService) Install(agent launchd.Agent) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.installed = append(service.installed, agent)
	return nil
}

func (service *fakeService) Remove(label string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.removed = append(service.removed, label)
	return nil
}

func (service *fakeService) State(string) string { return "running (pid 1)" }

type fakeCloudflared struct {
	mu       sync.Mutex
	calls    []string
	existing string
	routeErr bool
}

func (fake *fakeCloudflared) run(_ context.Context, name string, args ...string) ([]byte, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, strings.Join(args, " "))
	switch args[1] {
	case "list":
		if fake.existing != "" {
			return []byte(`2026-10-07T00:00:00Z INF note` + "\n" + `[{"id":"` + fake.existing + `","name":"overload"}]`), nil
		}
		return []byte(`[]`), nil
	case "create":
		for index, arg := range args {
			if arg == "--credentials-file" {
				_ = os.WriteFile(args[index+1], []byte(`{"secret":"x"}`), 0o600)
			}
		}
		return []byte(`{"id":"new-id","name":"overload"}`), nil
	case "route":
		if fake.routeErr {
			return []byte("ERR An A, AAAA, or CNAME record with that host already exists."), errors.New("exit status 1")
		}
		return []byte("INF Added CNAME"), nil
	case "login":
		return []byte("Please open the following URL and log in with your Cloudflare account:\n\nhttps://dash.cloudflare.com/argotunnel?aud=&callback=https%3A%2F%2Flogin.cloudflareaccess.org%2Fabc\n\nLeave cloudflared running to download the cert automatically.\nYou have successfully logged in."), nil
	}
	return nil, errors.New("unexpected " + strings.Join(args, " "))
}

func testManager(t *testing.T, fake *fakeCloudflared, probe func(context.Context, string) (int, error)) (*Manager, *fakeService) {
	t.Helper()
	home := t.TempDir()
	cert := filepath.Join(t.TempDir(), ".cloudflared", "cert.pem")
	if err := os.MkdirAll(filepath.Dir(cert), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &fakeService{}
	manager := &Manager{
		Home: home, Label: "dev.overload", LocalURL: "http://127.0.0.1:8082",
		Run: fake.run, Service: service, Probe: probe, Supported: true, CertPath: cert,
		LookPath: func(string) (string, error) { return "/opt/homebrew/bin/cloudflared", nil },
	}
	return manager, service
}

func wait(t *testing.T, manager *Manager) Status {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if status := manager.Status(); !status.Job.Running {
			return status
		}
	}
	t.Fatal("job did not finish")
	return Status{}
}

func reachable(_ context.Context, url string) (int, error) {
	if strings.HasSuffix(url, WebhookPath) {
		return 401, nil
	}
	return 404, nil
}

func TestSetupCreatesTunnelThatForwardsOnlyTheWebhook(t *testing.T) {
	fake := &fakeCloudflared{}
	manager, service := testManager(t, fake, reachable)
	var announced string
	manager.OnReady = func(_ context.Context, url string) (string, error) {
		announced = url
		return "Updated the GitHub App.", nil
	}
	if err := manager.Setup("overload", " Overload.Example.com "); err != nil {
		t.Fatal(err)
	}
	status := wait(t, manager)
	if status.Job.Err != "" {
		t.Fatalf("setup failed: %s\n%v", status.Job.Err, status.Job.Steps)
	}
	if status.Config != (Config{Name: "overload", ID: "new-id", Hostname: "overload.example.com"}) || announced != "https://overload.example.com/webhooks/github" || status.Job.Steps[len(status.Job.Steps)-1] != "Updated the GitHub App." {
		t.Fatalf("config %+v announced %q", status.Config, announced)
	}
	config, err := os.ReadFile(filepath.Join(manager.Home, "cloudflared", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tunnel: new-id", "hostname: overload.example.com", `path: ^/webhooks/github$`, "service: http://127.0.0.1:8082", "service: http_status:404", filepath.Join(manager.Home, "cloudflared", "new-id.json")} {
		if !strings.Contains(string(config), want) {
			t.Fatalf("config missing %q:\n%s", want, config)
		}
	}
	if _, err := os.Stat(filepath.Join(manager.Home, "cloudflared", "new-id.json")); err != nil {
		t.Fatalf("credentials not kept with the install: %v", err)
	}
	if len(service.installed) != 1 || service.installed[0].Label != "dev.overload.tunnel" || strings.Join(service.installed[0].Args[1:], " ") != "--no-autoupdate --config "+filepath.Join(manager.Home, "cloudflared", "config.yml")+" tunnel run new-id" {
		t.Fatalf("service %+v", service.installed)
	}
	if !strings.Contains(strings.Join(fake.calls, "\n"), "tunnel route dns new-id overload.example.com") {
		t.Fatalf("calls %q", fake.calls)
	}
}

func TestSetupReusesAnExistingTunnelWithLocalCredentials(t *testing.T) {
	fake := &fakeCloudflared{existing: "old-id"}
	manager, _ := testManager(t, fake, reachable)
	if err := manager.Setup("overload", "hooks.example.com"); err != nil {
		t.Fatal(err)
	}
	if status := wait(t, manager); !strings.Contains(status.Job.Err, "credentials are not on this Mac") {
		t.Fatalf("missing credentials not reported: %+v", status.Job)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(manager.CertPath), "old-id.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Setup("overload", "hooks.example.com"); err != nil {
		t.Fatal(err)
	}
	status := wait(t, manager)
	if status.Job.Err != "" || status.Config.ID != "old-id" || strings.Contains(strings.Join(fake.calls, "\n"), "create") {
		t.Fatalf("existing tunnel not reused: %+v %q", status, fake.calls)
	}
}

func TestSetupReportsFailures(t *testing.T) {
	fake := &fakeCloudflared{routeErr: true}
	manager, _ := testManager(t, fake, reachable)
	if err := manager.Setup("overload", "taken.example.com"); err != nil {
		t.Fatal(err)
	}
	if status := wait(t, manager); !strings.Contains(status.Job.Err, "record with that host already exists") {
		t.Fatalf("DNS conflict not reported: %+v", status.Job)
	}

	open := func(_ context.Context, url string) (int, error) {
		return map[bool]int{true: 401, false: 200}[strings.HasSuffix(url, WebhookPath)], nil
	}
	manager, _ = testManager(t, &fakeCloudflared{}, open)
	if err := manager.Setup("overload", "open.example.com"); err != nil {
		t.Fatal(err)
	}
	if status := wait(t, manager); !strings.Contains(status.Job.Err, "only the webhook should be reachable") {
		t.Fatalf("an exposed UI must fail the check: %+v", status.Job)
	}

	for _, bad := range []string{"", "localhost", "a b.com", "-x.example.com", "x.example.com/path"} {
		if err := manager.Setup("overload", bad); err == nil {
			t.Errorf("accepted hostname %q", bad)
		}
	}
	if err := manager.Setup("bad name", "ok.example.com"); err == nil {
		t.Error("accepted a tunnel name with a space")
	}
	unsupported := &Manager{}
	if err := unsupported.Setup("overload", "ok.example.com"); err == nil || !strings.Contains(err.Error(), "macOS") {
		t.Errorf("setup off macOS: %v", err)
	}
}

func TestLoginRecordsTheLink(t *testing.T) {
	manager, _ := testManager(t, &fakeCloudflared{}, reachable)
	if err := manager.Login(); err != nil {
		t.Fatal(err)
	}
	status := wait(t, manager)
	if status.Job.Err != "" || !strings.HasPrefix(status.Job.LoginURL, "https://dash.cloudflare.com/argotunnel?") {
		t.Fatalf("login %+v", status.Job)
	}
}
