// Package installtest runs `overload install` end to end on macOS with an
// isolated directory, launchd label and ports. It needs Homebrew Postgres
// (or --postgres-bin) and is skipped unless OVERLOAD_INSTALL_TEST=1.
package installtest

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type install struct {
	t      *testing.T
	binary string
	home   string
	label  string
	port   int
}

func TestNativeInstall(t *testing.T) {
	if os.Getenv("OVERLOAD_INSTALL_TEST") != "1" {
		t.Skip("set OVERLOAD_INSTALL_TEST=1 to install overload under a test label")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("the installer manages launchd services on macOS")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	inst := &install{t: t, binary: filepath.Join(t.TempDir(), "overload"), home: filepath.Join(t.TempDir(), "home"), label: fmt.Sprintf("dev.overload.installtest%d", time.Now().UnixNano()%1_000_000), port: freePort(t)}
	build := exec.Command("go", "build", "-o", inst.binary, "./cmd/overload")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := inst.run("uninstall", "--purge"); err != nil {
			t.Logf("cleanup: %v\n%s", err, out)
		}
	})

	out := inst.must("install", "--port", fmt.Sprint(inst.port))
	if !strings.Contains(out, "overload is running at") {
		t.Fatalf("install output:\n%s", out)
	}
	config := filepath.Join(inst.home, "overload.env")
	info, err := os.Stat(config)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config %s: %v %v", config, info, err)
	}
	env := readEnv(t, config)
	user, password := env["OVERLOAD_UI_USER"], env["OVERLOAD_UI_PASSWORD"]
	if user == "" || len(password) < 32 || !strings.Contains(env["DATABASE_URL"], "127.0.0.1") {
		t.Fatal("generated credentials missing or weak")
	}
	inst.expect("/healthz", "", "", http.StatusOK)
	inst.expect("/", "", "", http.StatusUnauthorized)
	inst.expect("/", user, "wrong", http.StatusUnauthorized)
	for _, path := range []string{"/", "/settings", "/runs", "/setup/github"} {
		inst.expect(path, user, password, http.StatusOK)
	}
	inst.must("install-check")
	inst.must("settings", "set", "--name", "install-test-model", "--url", "http://127.0.0.1:8000/v1", "--model", "test-model", "--default")

	for _, label := range []string{inst.label + ".postgres", inst.label} {
		if out, err := exec.Command("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).CombinedOutput(); err != nil {
			t.Fatalf("restart %s: %v\n%s", label, err, out)
		}
	}
	inst.waitHealthy()
	if body := inst.expect("/settings", user, password, http.StatusOK); !strings.Contains(body, "install-test-model") {
		t.Fatal("saved model missing after restarting both services")
	}

	inst.must("install")
	if readEnv(t, config)["OVERLOAD_UI_PASSWORD"] != password {
		t.Fatal("reinstall rewrote credentials")
	}
	inst.waitHealthy()
	inst.must("uninstall")
	if _, err := os.Stat(config); err != nil {
		t.Fatal("uninstall without --purge removed the configuration")
	}
	inst.must("install")
	if body := inst.expect("/settings", user, password, http.StatusOK); !strings.Contains(body, "install-test-model") {
		t.Fatal("data lost across uninstall and reinstall")
	}
	if status := inst.must("status"); !strings.Contains(status, "(healthy)") {
		t.Fatalf("status:\n%s", status)
	}
}

func (i *install) args(args ...string) []string {
	if args[0] == "install" || args[0] == "uninstall" || args[0] == "status" {
		return append(args, "--home", i.home, "--label", i.label)
	}
	return args
}

func (i *install) run(args ...string) (string, error) {
	cmd := exec.Command(i.binary, i.args(args...)...)
	cmd.Env = append(cleanEnv(), "OVERLOAD_CONFIG="+filepath.Join(i.home, "overload.env"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (i *install) must(args ...string) string {
	i.t.Helper()
	out, err := i.run(args...)
	if err != nil {
		i.t.Fatalf("overload %v: %v\n%s", args, err, out)
	}
	return out
}

func (i *install) waitHealthy() {
	i.t.Helper()
	for range 60 {
		if response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", i.port)); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Second)
	}
	i.t.Fatal("overload did not come back after restart")
}

func (i *install) expect(path, user, password string, status int) string {
	i.t.Helper()
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", i.port, path), nil)
	if err != nil {
		i.t.Fatal(err)
	}
	if user != "" {
		request.SetBasicAuth(user, password)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		i.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		i.t.Fatalf("GET %s as %q: status %d, want %d", path, user, response.StatusCode, status)
	}
	return string(body)
}

// cleanEnv drops settings from the caller's shell so the test uses only the
// installed configuration.
func cleanEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "DATABASE_URL=") || strings.HasPrefix(entry, "OVERLOAD_") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if key, value, ok := strings.Cut(scanner.Text(), "="); ok {
			values[key] = value
		}
	}
	return values
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
