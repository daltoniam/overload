package review_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/modelproxy"
	"github.com/daltoniam/overload/review"
	"github.com/daltoniam/overload/sandbox"
	gh "github.com/google/go-github/v75/github"
)

type archiveSource struct{ archive []byte }

func (source archiveSource) PullRequestWithDiff(_ context.Context, _ string, number int) (*gh.PullRequest, string, error) {
	pr := &gh.PullRequest{Number: gh.Ptr(number), State: gh.Ptr("open"), Head: &gh.PullRequestBranch{SHA: gh.Ptr(strings.Repeat("b", 40))}, Base: &gh.PullRequestBranch{SHA: gh.Ptr(strings.Repeat("a", 40))}}
	return pr, "diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old()\n+dangerous()\n", nil
}

func (source archiveSource) DownloadHead(_ context.Context, _, _ string, writer io.Writer) error {
	_, err := writer.Write(source.archive)
	return err
}

type recordingSandbox struct {
	sandbox.Local
	started, closed int
	names           []string
}

func (runner *recordingSandbox) Start(ctx context.Context, opts overload.SandboxOptions) (overload.Sandbox, error) {
	runner.started++
	box, err := runner.Local.Start(ctx, opts)
	return &recordingHandle{Sandbox: box, runner: runner}, err
}

type recordingHandle struct {
	overload.Sandbox
	runner *recordingSandbox
}

func (handle *recordingHandle) Upload(ctx context.Context, name string, reader io.Reader) error {
	handle.runner.names = append(handle.runner.names, name)
	return handle.Sandbox.Upload(ctx, name, reader)
}

func (handle *recordingHandle) Close(ctx context.Context) error {
	handle.runner.closed++
	return handle.Sandbox.Close(ctx)
}

func buildAgent(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, "overload-agent"), "../cmd/overload-agent")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func headArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	zip := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zip)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: "acme-api-bbbb/" + name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestSandboxRunnerReviewsInsideSandbox(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar required")
	}
	buildAgent(t)
	sawFile := false
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sawFile = sawFile || strings.Contains(string(body), "dangerous()")
		content := `{"summary":"Issue","findings":[{"path":"file.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bug","body":"Fix it","confidence":0.9,"evidence":"dangerous()"},{"path":"elsewhere.go","line":9,"side":"RIGHT","severity":"high","category":"bug","title":"Invented","body":"x","confidence":0.9,"evidence":"nope"}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, content)
	}))
	defer model.Close()
	entry := "Sandbox entry"
	workflow := overload.ResolvedWorkflow{Name: "sandbox", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "local", Model: overload.ModelProfile{Provider: "openaicompat", ConnectionKind: "local", BaseURL: model.URL, Model: "m"}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
	boxes := &recordingSandbox{}
	runner := review.SandboxRunner{Source: archiveSource{headArchive(t, map[string]string{"file.go": "dangerous()\n"})}, Sandbox: boxes, Workflow: workflow, RunID: 7}
	_, result, sha, err := runner.Review(context.Background(), "acme/api", 3)
	if err != nil || result.Error != "" {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	if sha != strings.Repeat("b", 40) || len(result.Findings) != 1 || result.Findings[0].Path != "file.go" {
		t.Fatalf("findings not validated against diff: %+v", result.Findings)
	}
	if boxes.started != 1 || boxes.closed != 1 || strings.Join(boxes.names, ",") != "spec.json,head.tar.gz" || !sawFile {
		t.Fatalf("started=%d closed=%d uploads=%v sawFile=%v", boxes.started, boxes.closed, boxes.names, sawFile)
	}
	if result.Metrics["sandbox"] != "local-7" {
		t.Fatalf("metrics %+v", result.Metrics)
	}
}

func TestSandboxRunnerRefusesCredentialedModels(t *testing.T) {
	entry := "x"
	workflow := overload.ResolvedWorkflow{Name: "hosted", Kind: "pr_review", Agents: []overload.ResolvedAgent{{Name: "hosted", Model: overload.ModelProfile{APIKeyEnv: "SECRET_KEY"}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
	boxes := &recordingSandbox{}
	_, _, _, err := review.SandboxRunner{Source: archiveSource{}, Sandbox: boxes, Workflow: workflow}.Review(context.Background(), "acme/api", 3)
	if err == nil || boxes.started != 0 {
		t.Fatalf("credentialed model ran in sandbox: %v", err)
	}
}

func TestSandboxRunnerClosesSandboxOnFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+os.Getenv("PATH"))
	entry := "x"
	workflow := overload.ResolvedWorkflow{Name: "w", Kind: "pr_review", Agents: []overload.ResolvedAgent{{Name: "a", Model: overload.ModelProfile{BaseURL: "http://127.0.0.1:1", Model: "m"}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
	boxes := &recordingSandbox{}
	_, _, _, err := review.SandboxRunner{Source: archiveSource{[]byte("not a tarball")}, Sandbox: boxes, Workflow: workflow}.Review(context.Background(), "acme/api", 3)
	if err == nil || boxes.started != 1 || boxes.closed != 1 {
		t.Fatalf("err=%v started=%d closed=%d", err, boxes.started, boxes.closed)
	}
}

func TestSandboxRunnerReachesKeyedModelsThroughProxy(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar required")
	}
	buildAgent(t)
	t.Setenv("OVERLOAD_MODEL_PROXY_TEST", "hosted-secret")
	var gotAuth string
	var specSawKey bool
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		content := `{"summary":"Issue","findings":[{"path":"file.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bug","body":"Fix it","confidence":0.9,"evidence":"dangerous()"}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"hosted-m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, content)
	}))
	defer model.Close()
	proxyServer := httptest.NewUnstartedServer(nil)
	proxy := modelproxy.New("http://" + proxyServer.Listener.Addr().String())
	proxyServer.Config.Handler = proxy
	proxyServer.Start()
	defer proxyServer.Close()
	entry := "Hosted entry"
	workflow := overload.ResolvedWorkflow{Name: "hosted", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "hosted", Model: overload.ModelProfile{Provider: "openaicompat", ConnectionKind: "hosted", BaseURL: model.URL, Model: "hosted-m", APIKeyEnv: "OVERLOAD_MODEL_PROXY_TEST"}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
	boxes := &specSandbox{onSpec: func(spec string) {
		specSawKey = strings.Contains(spec, "OVERLOAD_MODEL_PROXY_TEST") || strings.Contains(spec, model.URL)
	}}
	runner := review.SandboxRunner{Source: archiveSource{headArchive(t, map[string]string{"file.go": "dangerous()\n"})}, Sandbox: boxes, Workflow: workflow, RunID: 8, Models: proxy}
	_, result, _, err := runner.Review(context.Background(), "acme/api", 3)
	if err != nil || result.Error != "" || len(result.Findings) != 1 {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	if gotAuth != "Bearer hosted-secret" {
		t.Fatalf("model saw auth %q, want the key added by the proxy", gotAuth)
	}
	if specSawKey {
		t.Fatal("the sandbox spec named the key variable or the real model URL")
	}
	if runner.Workflow.Agents[0].Model.BaseURL != model.URL {
		t.Fatal("the run's pinned workflow was changed")
	}
}

type specSandbox struct {
	sandbox.Local
	onSpec func(string)
}

func (runner *specSandbox) Start(ctx context.Context, opts overload.SandboxOptions) (overload.Sandbox, error) {
	box, err := runner.Local.Start(ctx, opts)
	return &specHandle{Sandbox: box, onSpec: runner.onSpec}, err
}

type specHandle struct {
	overload.Sandbox
	onSpec func(string)
}

func (handle *specHandle) Upload(ctx context.Context, name string, reader io.Reader) error {
	if name == "spec.json" {
		raw, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		handle.onSpec(string(raw))
		reader = bytes.NewReader(raw)
	}
	return handle.Sandbox.Upload(ctx, name, reader)
}
