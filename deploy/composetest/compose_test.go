// Package composetest runs deploy/compose/compose.yaml under a throwaway
// Compose project. It needs Docker with Compose v2 and is skipped unless
// OVERLOAD_COMPOSE_TEST=1. The project gets its own name, port and volumes
// and is removed afterwards.
package composetest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type project struct {
	t       *testing.T
	name    string
	file    string
	envFile string
}

func TestComposeControlPlane(t *testing.T) {
	if os.Getenv("OVERLOAD_COMPOSE_TEST") != "1" {
		t.Skip("set OVERLOAD_COMPOSE_TEST=1 to run the Docker Compose test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker is required: %v", err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	user, password := "admin", secret(t)
	envFile := filepath.Join(t.TempDir(), "compose.env")
	env := fmt.Sprintf("OVERLOAD_POSTGRES_PASSWORD=%s\nOVERLOAD_UI_PASSWORD=%s\nOVERLOAD_PORT=%d\nOVERLOAD_IMAGE=overload-app:compose-test\n", secret(t), password, port)
	if err := os.WriteFile(envFile, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &project{t: t, name: "overload-compose-test-" + secret(t)[:8], file: filepath.Join(root, "deploy", "compose", "compose.yaml"), envFile: envFile}
	t.Cleanup(func() {
		if os.Getenv("OVERLOAD_COMPOSE_KEEP") == "1" {
			t.Logf("keeping compose project %s", p.name)
			return
		}
		if out, err := p.run("down", "--volumes", "--remove-orphans"); err != nil {
			t.Logf("compose down: %v\n%s", err, out)
		}
	})
	p.up("--build")
	if status := p.health(); status != "healthy" {
		t.Fatalf("container health is %q, want healthy", status)
	}

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	expect(t, base+"/healthz", "", "", http.StatusOK)
	expect(t, base+"/", "", "", http.StatusUnauthorized)
	expect(t, base+"/", user, "wrong", http.StatusUnauthorized)
	for _, path := range []string{"/", "/settings", "/runs", "/catalog"} {
		expect(t, base+path, user, password, http.StatusOK)
	}
	p.must("exec", "-T", "overload", "/usr/local/bin/overload", "install-check")
	p.must("exec", "-T", "overload", "/usr/local/bin/overload", "settings", "set", "--name", "compose-test-model", "--url", "http://model.invalid/v1", "--model", "test-model", "--default")

	// A stop sends SIGTERM; the server should shut down cleanly rather than
	// be killed when the grace period runs out.
	started := time.Now()
	p.must("stop", "--timeout", "30", "overload")
	if elapsed := time.Since(started); elapsed > 25*time.Second {
		t.Fatalf("stopping the app took %s; SIGTERM was not handled", elapsed)
	}
	if code := strings.TrimSpace(p.inspect("{{.State.ExitCode}}")); code != "0" {
		t.Fatalf("app exited with code %s after SIGTERM, want 0", code)
	}

	p.must("down")
	p.up()
	if body := expect(t, base+"/settings", user, password, http.StatusOK); !strings.Contains(body, "compose-test-model") {
		t.Fatal("saved model missing after the project was recreated")
	}
}

func (p *project) up(args ...string) {
	p.t.Helper()
	args = append([]string{"up", "--detach", "--wait", "--wait-timeout", "300"}, args...)
	if out, err := p.run(args...); err != nil {
		logs, _ := p.run("logs", "--tail", "60")
		p.t.Fatalf("compose up: %v\n%s\n%s", err, out, logs)
	}
}

func (p *project) health() string {
	p.t.Helper()
	return strings.TrimSpace(p.inspect("{{if .State.Health}}{{.State.Health.Status}}{{end}}"))
}

func (p *project) inspect(format string) string {
	p.t.Helper()
	id := strings.TrimSpace(p.must("ps", "--all", "--quiet", "overload"))
	if id == "" {
		p.t.Fatal("no overload container")
	}
	out, err := exec.Command("docker", "inspect", "--format", format, id).CombinedOutput()
	if err != nil {
		p.t.Fatalf("docker inspect: %v\n%s", err, out)
	}
	return string(out)
}

func (p *project) must(args ...string) string {
	p.t.Helper()
	out, err := p.run(args...)
	if err != nil {
		p.t.Fatalf("compose %s: %v\n%s", args[0], err, out)
	}
	return out
}

func (p *project) run(args ...string) (string, error) {
	cmd := exec.Command("docker", append([]string{"compose", "--project-name", p.name, "--file", p.file, "--env-file", p.envFile}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func expect(t *testing.T, url, user, password string, status int) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		request.SetBasicAuth(user, password)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		t.Fatalf("GET %s as %q: status %d, want %d", url, user, response.StatusCode, status)
	}
	return string(body)
}

func secret(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}
