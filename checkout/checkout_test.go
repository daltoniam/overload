package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daltoniam/overload"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package app\n\nfunc ok() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package app\n\nfunc ok() {}\n\nfunc broken() { bug() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-q", "-am", "add broken")
	return dir
}

// fakeModels plays the reviewer (a finding while bug() is in the diff) and
// the fixer (writes a fixed app.go with one tool call, then reports).
func fakeModels(t *testing.T, fixes *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		text := fmt.Sprint(request.Messages)
		w.Header().Set("Content-Type", "application/json")
		reply := func(message string) {
			_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, request.Model, message)
		}
		if request.Model == "fixer" {
			if request.Messages[len(request.Messages)-1].Role == "tool" {
				reply("Fixed broken by removing bug().")
				return
			}
			fixes.Add(1)
			arguments, _ := json.Marshal(map[string]string{"path": "app.go", "content": "package app\n\nfunc ok() {}\n\nfunc broken() {}\n"})
			_, _ = fmt.Fprintf(w, `{"id":"2","object":"chat.completion","created":1,"model":"fixer","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"write_file","arguments":%q}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, arguments)
			return
		}
		if strings.Contains(text, "+func broken() { bug() }") {
			reply(`{"summary":"bug","findings":[{"path":"app.go","line":5,"side":"RIGHT","severity":"high","category":"bug","title":"Calls bug","body":"bug() panics","confidence":0.9,"evidence":"func broken() { bug() }"}]}`)
			return
		}
		reply(`{"summary":"No actionable findings in reviewed files.","findings":[]}`)
	}))
}

func workflowFor(url string) overload.ResolvedWorkflow {
	body := "Review."
	return overload.ResolvedWorkflow{Version: overload.SnapshotVersion, Name: "local", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "main", Model: overload.ModelProfile{Provider: "openaicompat", BaseURL: url, Model: "reviewer", Concurrency: 1}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: body, SHA256: overload.PromptDigest(body)}}}}
}

func TestReviewAndReport(t *testing.T) {
	dir := gitRepo(t)
	var fixes atomic.Int64
	server := fakeModels(t, &fixes)
	defer server.Close()
	ctx := context.Background()
	if _, err := Diff(ctx, dir, "--upload-pack=x"); err == nil {
		t.Fatal("an option was accepted as the base")
	}
	result, err := Review(ctx, dir, "main", workflowFor(server.URL))
	if err != nil || len(result.Findings) != 1 {
		t.Fatalf("review = %+v, %v", result, err)
	}
	var report bytes.Buffer
	if err := Report(&report, workflowFor(server.URL), result); err != nil || !strings.Contains(report.String(), "app.go:5 [high] Calls bug") {
		t.Fatalf("report: %v\n%s", err, report.String())
	}
	if _, err := Review(ctx, dir, "feature", workflowFor(server.URL)); err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("review with no changes: %v", err)
	}
}

func TestFixLoop(t *testing.T) {
	dir := gitRepo(t)
	var fixes atomic.Int64
	server := fakeModels(t, &fixes)
	defer server.Close()
	var out bytes.Buffer
	fixer := overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "fixer", Concurrency: 1}
	result, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, TestCommand: "grep -q 'func broken() {}' app.go", Rounds: 2, Until: "high", Out: &out, now: func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatalf("fix: %v\n%s", err, out.String())
	}
	if !result.Clean || result.Rounds != 1 || fixes.Load() != 1 || result.TestsPassed == nil || !*result.TestsPassed || result.Branch != "overload/fix-20261109-120000" {
		t.Fatalf("result = %+v\n%s", result, out.String())
	}
	original, _ := os.ReadFile(filepath.Join(dir, "app.go"))
	if !strings.Contains(string(original), "bug()") {
		t.Fatal("the fix changed the original working tree")
	}
	log, err := Git(context.Background(), dir, "log", "--format=%s", result.Branch)
	if err != nil || !strings.Contains(log, "overload: fix review findings (round 1)") {
		t.Fatalf("branch log: %q %v", log, err)
	}
	if _, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, Rounds: 0, Until: "high"}); err == nil {
		t.Fatal("zero rounds accepted")
	}
}

func TestFixerToolsStayInTheWorktree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	tools := map[string]*localTool{}
	for _, tool := range fixerTools(root, dir, "") {
		tools[tool.Info().Name] = tool.(*localTool)
	}
	ctx := context.Background()
	if text, err := tools["read_file"].run(ctx, map[string]any{"path": "a.txt"}); err != nil || text != "hello" {
		t.Fatalf("read: %q %v", text, err)
	}
	for _, path := range []string{"../outside", "/etc/passwd", ".git/config", "escape/passwd"} {
		if _, err := tools["read_file"].run(ctx, map[string]any{"path": path}); err == nil {
			t.Errorf("read %s", path)
		}
		if _, err := tools["write_file"].run(ctx, map[string]any{"path": path, "content": "x"}); err == nil {
			t.Errorf("wrote %s", path)
		}
	}
	if _, err := tools["write_file"].run(ctx, map[string]any{"path": "new/dir/b.txt", "content": "made"}); err != nil {
		t.Fatal(err)
	}
	if listing, err := tools["list_files"].run(ctx, map[string]any{}); err != nil || !strings.Contains(listing, "new/") {
		t.Fatalf("list: %q %v", listing, err)
	}
	if _, ok := tools["run_tests"]; ok {
		t.Fatal("run_tests offered without a test command")
	}
}
