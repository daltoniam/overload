package kindtest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const agentSandboxRelease = "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox-with-extensions.yaml"

// Public, open PR used as review input. Override with OVERLOAD_KIND_PR as
// owner/repo#number when it closes.
const defaultSandboxPR = "daltoniam/switchboard#219"

func TestKindSandboxReview(t *testing.T) {
	if os.Getenv("OVERLOAD_KIND_SANDBOX_TEST") != "1" {
		t.Skip("set OVERLOAD_KIND_SANDBOX_TEST=1 to run the kind Agent Sandbox review test")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		out, err := exec.Command("gh", "auth", "token").Output()
		if err != nil {
			t.Skip("GITHUB_TOKEN or gh auth required to read the public test PR")
		}
		token = strings.TrimSpace(string(out))
	}
	target := os.Getenv("OVERLOAD_KIND_PR")
	if target == "" {
		target = defaultSandboxPR
	}
	repo, number, ok := strings.Cut(target, "#")
	if !ok {
		t.Fatalf("OVERLOAD_KIND_PR must be owner/repo#number")
	}
	head, base := prSHAs(t, repo, number)

	c := newCluster(t, "overload-sandbox-test")
	c.kubectl("apply", "--server-side", "-f", agentSandboxRelease)
	c.kubectl("-n", "agent-sandbox-system", "rollout", "status", "deployment/agent-sandbox-controller", "--timeout=180s")
	c.loadImage("overload-app:local", "deploy/images/app/Dockerfile")
	c.loadImage("overload-agent:dev", "deploy/images/agent/Dockerfile")

	webhookSecret := secret(t)
	user, password := c.installApp("kind-sandbox", map[string]string{"GITHUB_TOKEN": token, "GITHUB_WEBHOOK_SECRET": webhookSecret})
	c.kubectl("apply", "-f", filepath.Join(c.root, "deploy", "kindtest", "testdata", "fake-model.yaml"))
	c.kubectl("-n", "overload", "rollout", "status", "deployment/fake-model", "--timeout=180s")
	c.waitWarmPool()

	agentPod := strings.TrimSpace(c.kubectl("-n", "overload", "get", "pods", "-l", "app.kubernetes.io/name=overload-agent", "-o", "jsonpath={.items[0].metadata.name}"))
	if out, err := c.run("kubectl", "--kubeconfig", c.kubeconfig, "-n", "overload", "exec", agentPod, "--", "bash", "-c", "timeout 5 bash -c '</dev/tcp/fake-model.overload.svc/8080'"); err != nil {
		t.Fatalf("sandbox pod cannot reach the model: %v\n%s", err, out)
	}
	for _, blocked := range []string{"postgres.overload.svc/5432", "overload.overload.svc/8082", "kubernetes.default.svc/443", "1.1.1.1/443", "api.github.com/443"} {
		if out, err := c.run("kubectl", "--kubeconfig", c.kubeconfig, "-n", "overload", "exec", agentPod, "--", "bash", "-c", "timeout 5 bash -c '</dev/tcp/"+blocked+"'"); err == nil {
			t.Fatalf("sandbox pod reached %s; egress policy not enforced\n%s", blocked, out)
		}
	}

	exec := func(stdin string, args ...string) {
		t.Helper()
		cmd := exec.Command("kubectl", append([]string{"--kubeconfig", c.kubeconfig, "--context", "kind-" + c.name, "-n", "overload", "exec", "-i", "deploy/overload", "--", "/usr/local/bin/overload"}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("overload %v: %v\n%s", args, err, out)
		}
	}
	exec("", "settings", "set", "--name", "fake", "--url", "http://fake-model.overload.svc:8080/v1", "--model", "fake", "--default")
	exec(`{"name":"sandbox-entry","kind":"entry","body":"Review the pull request for concrete bugs."}`, "prompts", "apply", "-")
	exec(`{"name":"sandbox-reviewer","model":"fake","entry_prompt":"sandbox-entry","enabled":true}`, "agents", "apply", "-")
	exec(`{"name":"sandbox-review","kind":"pr_review","agents":["sandbox-reviewer"],"enabled":true}`, "workflows", "apply", "-")
	exec(`{"name":"`+repo+`","enabled":true,"dry_run":true}`, "repositories", "apply", "-")
	exec(`{"source":"github","event":"pull_request","action":"opened","repository":"`+repo+`","workflow":"sandbox-review","enabled":true}`, "bindings", "apply", "-")

	payload := fmt.Sprintf(`{"action":"opened","number":%s,"repository":{"full_name":%q},"pull_request":{"draft":false,"head":{"sha":%q},"base":{"sha":%q},"user":{"type":"User"}}}`, number, repo, head, base)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	_, _ = mac.Write([]byte(payload))
	baseURL, stop := c.portForward()
	defer stop()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/webhooks/github", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-GitHub-Event", "pull_request")
	request.Header.Set("X-GitHub-Delivery", "kind-sandbox-"+secret(t)[:12])
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook status %d", response.StatusCode)
	}

	var row string
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		row = strings.TrimSpace(c.kubectl("-n", "overload", "exec", "postgres-0", "--", "psql", "-U", "overload", "-d", "overload", "-Atc", "SELECT status||'|'||sandbox_name||'|'||error_code||'|'||error_message FROM runs ORDER BY id DESC LIMIT 1"))
		if strings.HasPrefix(row, "completed|") || strings.HasPrefix(row, "failed|") {
			break
		}
		time.Sleep(5 * time.Second)
	}
	logs := c.kubectl("-n", "overload", "logs", "deploy/fake-model")
	if !strings.HasPrefix(row, "completed|") {
		t.Fatalf("run did not complete: %q\napp logs:\n%s\nmodel logs:\n%s", row, c.kubectl("-n", "overload", "logs", "deploy/overload", "--tail=50"), logs)
	}
	fields := strings.Split(row, "|")
	if len(fields) < 2 || fields[1] == "" {
		t.Fatalf("run has no sandbox name: %q", row)
	}
	if !strings.Contains(logs, "effort=xhigh has_diff=True") {
		t.Fatalf("model never saw a sandboxed xhigh review request:\n%s", logs)
	}
	t.Logf("run %s reviewed in sandbox %s", row, fields[1])
	expect(t, baseURL+"/runs", user, password, http.StatusOK)

	waitFor(t, 2*time.Minute, func() bool {
		claims := strings.TrimSpace(c.kubectl("-n", "overload", "get", "sandboxclaims", "-o", "name"))
		return claims == ""
	}, "sandbox claim was not deleted after the review")
}

func prSHAs(t *testing.T, repo, number string) (string, string) {
	t.Helper()
	out, err := exec.Command("gh", "pr", "view", number, "-R", repo, "--json", "headRefOid,baseRefOid,state", "-q", ".state+\" \"+.headRefOid+\" \"+.baseRefOid").Output()
	if err != nil {
		t.Fatalf("look up %s#%s: %v", repo, number, err)
	}
	parts := strings.Fields(string(out))
	if len(parts) != 3 || parts[0] != "OPEN" {
		t.Skipf("%s#%s is not open; set OVERLOAD_KIND_PR", repo, number)
	}
	return parts[1], parts[2]
}

func (c *cluster) waitWarmPool() {
	c.t.Helper()
	waitFor(c.t, 5*time.Minute, func() bool {
		out, _ := c.run("kubectl", "--kubeconfig", c.kubeconfig, "-n", "overload", "get", "pods", "-l", "app.kubernetes.io/name=overload-agent", "-o", "jsonpath={.items[*].status.containerStatuses[*].ready}")
		return strings.Contains(out, "true")
	}, "no ready warm-pool sandbox pod")
}

func waitFor(t *testing.T, timeout time.Duration, ready func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatal(message)
}
