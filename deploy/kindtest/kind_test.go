// Package kindtest installs the kind overlay into a disposable kind cluster.
// It needs Docker, kind and kubectl and is skipped unless OVERLOAD_KIND_TEST=1.
// The cluster gets its own kubeconfig file, so the caller's kubeconfig and
// current context are never read or modified.
package kindtest

import (
	"context"
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

type cluster struct {
	t          *testing.T
	root       string
	name       string
	kubeconfig string
}

// newCluster creates a disposable kind cluster with its own kubeconfig file
// and deletes it when the test ends.
func newCluster(t *testing.T, name string) *cluster {
	t.Helper()
	for _, tool := range []string{"docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required: %v", tool, err)
		}
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	c := &cluster{t: t, root: root, name: name, kubeconfig: filepath.Join(t.TempDir(), "kubeconfig")}
	if out, _ := c.run("kind", "get", "clusters"); strings.Contains(out, name) {
		t.Fatalf("kind cluster %s already exists; delete it first", name)
	}
	c.must("kind", "create", "cluster", "--name", name, "--kubeconfig", c.kubeconfig, "--config", filepath.Join(root, "deploy", "kind", "cluster.yaml"), "--wait", "120s")
	t.Cleanup(func() {
		if os.Getenv("OVERLOAD_KIND_KEEP") == "1" {
			t.Logf("keeping cluster; kubeconfig %s", c.kubeconfig)
			return
		}
		if out, err := c.run("kind", "delete", "cluster", "--name", name, "--kubeconfig", c.kubeconfig); err != nil {
			t.Logf("delete cluster: %v\n%s", err, out)
		}
	})
	if out := c.kubectl("config", "current-context"); strings.TrimSpace(out) != "kind-"+name {
		t.Fatalf("unexpected context %q", out)
	}
	return c
}

func (c *cluster) loadImage(tag, dockerfile string) {
	c.t.Helper()
	c.must("docker", "build", "-f", filepath.Join(c.root, dockerfile), "-t", tag, c.root)
	c.must("kind", "load", "docker-image", tag, "--name", c.name)
}

// installApp creates secrets and applies an overlay. extra adds keys to the
// app secret.
func (c *cluster) installApp(overlay string, extra map[string]string) (string, string) {
	c.t.Helper()
	dbPassword, user, password := secret(c.t), "admin", secret(c.t)
	c.kubectl("create", "namespace", "overload")
	c.kubectl("-n", "overload", "create", "secret", "generic", "overload-postgres", "--from-literal=password="+dbPassword)
	args := []string{"-n", "overload", "create", "secret", "generic", "overload",
		"--from-literal=DATABASE_URL=postgres://overload:" + dbPassword + "@postgres.overload.svc:5432/overload?sslmode=disable",
		"--from-literal=OVERLOAD_UI_USER=" + user, "--from-literal=OVERLOAD_UI_PASSWORD=" + password}
	for key, value := range extra {
		args = append(args, "--from-literal="+key+"="+value)
	}
	c.kubectl(args...)
	c.kubectl("apply", "-k", filepath.Join(c.root, "deploy", "k8s", "overlays", overlay))
	c.waitReady()
	return user, password
}

func TestKindInstall(t *testing.T) {
	if os.Getenv("OVERLOAD_KIND_TEST") != "1" {
		t.Skip("set OVERLOAD_KIND_TEST=1 to run the disposable kind install test")
	}
	c := newCluster(t, "overload-install-test")
	c.loadImage("overload-app:local", "deploy/images/app/Dockerfile")
	user, password := c.installApp("kind", nil)

	base, stop := c.portForward()
	expect(t, base+"/healthz", "", "", http.StatusOK)
	expect(t, base+"/", "", "", http.StatusUnauthorized)
	expect(t, base+"/", user, "wrong", http.StatusUnauthorized)
	for _, path := range []string{"/", "/settings", "/runs", "/setup/github"} {
		expect(t, base+path, user, password, http.StatusOK)
	}
	c.kubectl("-n", "overload", "exec", "deploy/overload", "--", "/usr/local/bin/overload", "install-check")
	c.kubectl("-n", "overload", "exec", "deploy/overload", "--", "/usr/local/bin/overload", "settings", "set", "--name", "kind-test-model", "--url", "http://model.invalid/v1", "--model", "test-model", "--default")
	stop()

	c.kubectl("-n", "overload", "delete", "pod", "postgres-0", "--wait=true")
	c.kubectl("-n", "overload", "rollout", "restart", "deployment/overload")
	c.waitReady()
	base, stop = c.portForward()
	defer stop()
	if body := expect(t, base+"/settings", user, password, http.StatusOK); !strings.Contains(body, "kind-test-model") {
		t.Fatal("saved model missing after Postgres and app restart")
	}
}

func (c *cluster) waitReady() {
	c.t.Helper()
	for _, target := range []string{"statefulset/postgres", "deployment/overload"} {
		if out, err := c.run("kubectl", "--kubeconfig", c.kubeconfig, "--context", "kind-"+c.name, "-n", "overload", "rollout", "status", target, "--timeout=300s"); err != nil {
			pods, _ := c.run("kubectl", "--kubeconfig", c.kubeconfig, "--context", "kind-"+c.name, "-n", "overload", "get", "pods,events", "-o", "wide")
			logs, _ := c.run("kubectl", "--kubeconfig", c.kubeconfig, "--context", "kind-"+c.name, "-n", "overload", "logs", target, "--tail=40", "--all-containers")
			c.t.Fatalf("%s not ready: %v\n%s\n%s\n%s", target, err, out, pods, logs)
		}
	}
}

func (c *cluster) portForward() (string, func()) {
	c.t.Helper()
	port := freePort(c.t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", c.kubeconfig, "--context", "kind-"+c.name, "-n", "overload", "port-forward", "svc/overload", fmt.Sprintf("%d:8082", port))
	if err := cmd.Start(); err != nil {
		cancel()
		c.t.Fatal(err)
	}
	stop := func() { cancel(); _ = cmd.Wait() }
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	for range 60 {
		if response, err := http.Get(base + "/healthz"); err == nil {
			_ = response.Body.Close()
			return base, stop
		}
		time.Sleep(time.Second)
	}
	stop()
	c.t.Fatal("port-forward never became ready")
	return "", nil
}

func (c *cluster) kubectl(args ...string) string {
	c.t.Helper()
	return c.must("kubectl", append([]string{"--kubeconfig", c.kubeconfig, "--context", "kind-" + c.name}, args...)...)
}

func (c *cluster) must(name string, args ...string) string {
	c.t.Helper()
	out, err := c.run(name, args...)
	if err != nil {
		c.t.Fatalf("%s %s: %v\n%s", name, args[0], err, out)
	}
	return out
}

func (c *cluster) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+c.kubeconfig)
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
