package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	return fakeModelsWriting(t, fixes, "app.go", "package app\n\nfunc ok() {}\n\nfunc broken() {}\n")
}

// fakeModelsWriting is fakeModels with a fixer that writes content to
// path.
func fakeModelsWriting(t *testing.T, fixes *atomic.Int64, path, content string) *httptest.Server {
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
			arguments, _ := json.Marshal(map[string]string{"path": path, "content": content})
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
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hooks := filepath.Join(dir, ".git", "hooks")
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, TestCommand: "grep -q 'func broken() {}' app.go && touch build.out", Rounds: 2, Until: "high", Out: &out, now: func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatalf("fix: %v\n%s", err, out.String())
	}
	if !result.Clean || result.Rounds != 1 || fixes.Load() != 1 || result.TestsPassed == nil || !*result.TestsPassed || !strings.HasPrefix(result.Branch, "overload/fix-20261109-120000-") {
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
	if files, err := Git(context.Background(), dir, "show", "--format=", "--name-only", result.Branch); err != nil || strings.TrimSpace(files) != "app.go" {
		t.Fatalf("the fix commit holds %q, want only the fixer's file: %v", files, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repository hook ran during the fix loop")
	}
	if _, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, Rounds: 0, Until: "high"}); err == nil {
		t.Fatal("zero rounds accepted")
	}
}

func TestWorkPathRefusesGitFiles(t *testing.T) {
	for _, test := range []struct {
		raw, want string
	}{
		{"app.go", "app.go"},
		{"./web/app.go", "web/app.go"},
		{"", "."},
		{".github/workflows/ci.yml", ".github/workflows/ci.yml"},
		{".gitignore", ".gitignore"},
		{"docs/git/notes.md", "docs/git/notes.md"},
		{".git", ""},
		{".git/config", ""},
		{".GIT", ""},
		{".GIT/HEAD", ""},
		{".Git/config", ""},
		{"sub/.git", ""},
		{"sub/.git/config", ""},
		{"sub/.GIT/hooks/pre-commit", ""},
		{".git.", ""},
		{".git /config", ""},
		{".g\u200cit/config", ""},
		{"\ufeff.git", ""},
		{"GIT~1/config", ""},
		{"a/../.git/config", ""},
		{"../outside", ""},
		{"/etc/passwd", ""},
	} {
		got, err := workPath(test.raw)
		if test.want == "" && err == nil {
			t.Errorf("workPath(%q) = %q, want refused", test.raw, got)
		}
		if test.want != "" && (err != nil || got != test.want) {
			t.Errorf("workPath(%q) = %q, %v, want %q", test.raw, got, err, test.want)
		}
	}
}

func TestFixerToolsStayInTheWorktree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := []byte("gitdir: /somewhere/.git/worktrees/fix\n")
	if err := os.WriteFile(filepath.Join(dir, ".git"), gitFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub", ".git"), 0o755); err != nil {
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
	var written writtenPaths
	tools := map[string]*localTool{}
	for _, tool := range fixerTools(root, dir, "", &written) {
		tools[tool.Info().Name] = tool.(*localTool)
	}
	ctx := context.Background()
	if text, err := tools["read_file"].run(ctx, map[string]any{"path": "a.txt"}); err != nil || text != "hello" {
		t.Fatalf("read: %q %v", text, err)
	}
	for _, path := range []string{"../outside", "/etc/passwd", ".git", ".git/config", ".GIT", ".GIT/HEAD", ".Git/config", "sub/.git/config", "sub/.GIT/config", "sub/.GIT", ".git.", "escape/passwd"} {
		if _, err := tools["read_file"].run(ctx, map[string]any{"path": path}); err == nil {
			t.Errorf("read %s", path)
		}
		if _, err := tools["write_file"].run(ctx, map[string]any{"path": path, "content": "gitdir: /tmp/evil"}); err == nil {
			t.Errorf("wrote %s", path)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, ".git")); err != nil || !bytes.Equal(data, gitFile) {
		t.Fatalf("the worktree's .git file changed: %q %v", data, err)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "sub", ".git")); err != nil || len(entries) != 0 {
		t.Fatalf("wrote into sub/.git: %v %v", entries, err)
	}
	if _, err := tools["write_file"].run(ctx, map[string]any{"path": "new/dir/b.txt", "content": "made"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools["write_file"].run(ctx, map[string]any{"path": "run.sh", "content": "#!/bin/sh\necho hi\n"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("rewriting an executable changed its mode: %v %v", info.Mode(), err)
	}
	if got := strings.Join(written.list(), ","); got != "new/dir/b.txt,run.sh" {
		t.Fatalf("written = %s", got)
	}
	listing, err := tools["list_files"].run(ctx, map[string]any{})
	if err != nil || !strings.Contains(listing, "new/") || strings.Contains(listing, ".git") {
		t.Fatalf("list: %q %v", listing, err)
	}
	if _, ok := tools["run_tests"]; ok {
		t.Fatal("run_tests offered without a test command")
	}
}

func TestReviewDoesNotFollowSymlinksOutOfTheCheckout(t *testing.T) {
	dir := gitRepo(t)
	secret := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(secret, []byte("aws_secret_access_key = SECRET-CANARY-1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Git(context.Background(), dir, "add", "notes.txt"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"reviewer","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"summary\":\"ok\",\"findings\":[]}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer server.Close()
	if _, err := Review(context.Background(), dir, "main", workflowFor(server.URL)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no review requests")
	}
	for _, body := range bodies {
		if strings.Contains(body, "SECRET-CANARY-1234") {
			t.Fatal("a file outside the checkout was sent to the model through a symbolic link")
		}
	}
}

func TestReviewIgnoresUserDiffSettingsAndSubdirectories(t *testing.T) {
	dir := gitRepo(t)
	for _, setting := range [][2]string{{"diff.noprefix", "true"}, {"diff.mnemonicPrefix", "true"}, {"diff.relative", "true"}, {"diff.external", "false"}} {
		if _, err := Git(context.Background(), dir, "config", setting[0], setting[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	var fixes atomic.Int64
	server := fakeModels(t, &fixes)
	defer server.Close()
	result, err := Review(context.Background(), filepath.Join(dir, "web"), "main", workflowFor(server.URL))
	if err != nil || len(result.Findings) != 1 || result.Findings[0].Path != "app.go" {
		t.Fatalf("review from a subdirectory = %+v, %v", result, err)
	}
}

func TestGitIgnoresInheritedRepositoryAndHooks(t *testing.T) {
	dir := gitRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "index"))
	if head, err := Git(context.Background(), dir, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || strings.TrimSpace(head) != "feature" {
		t.Fatalf("HEAD = %q, %v", head, err)
	}
	if _, err := Git(context.Background(), dir, "frobnicate"); err == nil || !strings.HasPrefix(err.Error(), "git frobnicate:") {
		t.Fatalf("error label: %v", err)
	}
}

func TestFixNeedsCommittedChanges(t *testing.T) {
	dir := gitRepo(t)
	if _, err := Git(context.Background(), dir, "checkout", "-q", "-b", "uncommitted", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package app\n\nfunc ok() { bug() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var fixes atomic.Int64
	server := fakeModels(t, &fixes)
	defer server.Close()
	fixer := overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "fixer", Concurrency: 1}
	_, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, Rounds: 1, Until: "high"})
	if err == nil || !strings.Contains(err.Error(), "no committed changes") {
		t.Fatalf("fix with only uncommitted changes: %v", err)
	}
	assertNoFixBranches(t, dir)
}

func TestFixRemovesBranchWhenFirstRoundFails(t *testing.T) {
	dir := gitRepo(t)
	var fixes atomic.Int64
	server := fakeModels(t, &fixes)
	defer server.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer down.Close()
	fixer := overload.ModelProfile{Provider: "openaicompat", BaseURL: down.URL, Model: "fixer", Concurrency: 1}
	var out bytes.Buffer
	result, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, Rounds: 1, Until: "high", Out: &out})
	if err == nil || !strings.Contains(err.Error(), "fix round 1") {
		t.Fatalf("fix with a failing fixer: %v\n%s", err, out.String())
	}
	if result.Branch != "" || !strings.Contains(out.String(), "removed branch") {
		t.Fatalf("result = %+v\n%s", result, out.String())
	}
	assertNoFixBranches(t, dir)
}

func assertNoFixBranches(t *testing.T, dir string) {
	t.Helper()
	branches, err := Git(context.Background(), dir, "branch", "--list", "overload/fix-*")
	if err != nil || strings.TrimSpace(branches) != "" {
		t.Fatalf("fix branches left: %q %v", branches, err)
	}
	worktrees, err := Git(context.Background(), dir, "worktree", "list", "--porcelain")
	if err != nil || strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("worktrees left: %q %v", worktrees, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, ".git", "overload-fix")); len(entries) != 0 {
		t.Fatalf("worktree directories left: %v", entries)
	}
}

func TestRunTestsEnvironmentAndTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	t.Setenv("OVERLOAD_DEFAULT_MODEL_API_KEY", "sk-secret")
	t.Setenv("GITHUB_TOKEN", "ghp-secret")
	t.Setenv("GOFLAGS", "-count=1")
	t.Setenv("GOPROXY", "https://user:token@proxy.example.com")
	t.Setenv("GONOSUMDB", "git.example.com/*")
	passed, output := RunTests(context.Background(), t.TempDir(), `test -z "$DATABASE_URL" && test -z "$OVERLOAD_DEFAULT_MODEL_API_KEY" && test -z "$GITHUB_TOKEN" && test -z "$GOPROXY" && test -n "$PATH" && test -n "$HOME" && test "$GOFLAGS" = -count=1 && test "$GONOSUMDB" = "git.example.com/*"`)
	if !passed {
		t.Fatalf("tests saw secrets or lost toolchain variables: %s", output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if passed, _ := RunTests(ctx, t.TempDir(), "sleep 60 & sleep 60"); passed {
		t.Fatal("a cancelled test run passed")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("cancelling tests took %s; a background process kept them running", elapsed)
	}
}

func TestFixerToolsRefuseSymlinks(t *testing.T) {
	dir := t.TempDir()
	gitFile := []byte("gitdir: /somewhere/.git/worktrees/fix\n")
	if err := os.WriteFile(filepath.Join(dir, ".git"), gitFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"cfg": ".git", "folder": "real", "dangling": "missing.txt"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var written writtenPaths
	tools := map[string]*localTool{}
	for _, tool := range fixerTools(root, dir, "", &written) {
		tools[tool.Info().Name] = tool.(*localTool)
	}
	for _, path := range []string{"cfg", "folder/a.txt", "folder/new/b.txt", "dangling"} {
		if _, err := tools["write_file"].run(context.Background(), map[string]any{"path": path, "content": "gitdir: /tmp/evil"}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("write through %s: %v", path, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, ".git")); err != nil || !bytes.Equal(data, gitFile) {
		t.Fatalf("the .git file changed through a link: %q %v", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "real")); len(entries) != 0 || len(written.list()) != 0 {
		t.Fatalf("wrote through a folder link: %v %v", entries, written.list())
	}
	if _, err := tools["write_file"].run(context.Background(), map[string]any{"path": "real/a.txt", "content": "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "absolute")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		tool, path, mentions string
	}{
		{"read_file", "cfg", ""},
		{"read_file", "folder/a.txt", "symbolic link to real"},
		{"read_file", "absolute/a.txt", ""},
		{"list_files", "cfg", ""},
		{"list_files", "folder", "symbolic link to real"},
	} {
		text, err := tools[test.tool].run(context.Background(), map[string]any{"path": test.path})
		if err == nil {
			t.Errorf("%s %s through a link: %q", test.tool, test.path, text)
			continue
		}
		if strings.Contains(err.Error(), "gitdir") || strings.Contains(err.Error(), ".git") || strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), test.mentions) {
			t.Errorf("%s %s: %v", test.tool, test.path, err)
		}
	}
	listing, err := tools["list_files"].run(context.Background(), map[string]any{})
	if err != nil || !strings.Contains(listing, "cfg@") || !strings.Contains(listing, "real/") || strings.Contains(listing, ".git") {
		t.Fatalf("list: %q %v", listing, err)
	}
	if text, err := tools["read_file"].run(context.Background(), map[string]any{"path": "real/a.txt"}); err != nil || text != "ok" {
		t.Fatalf("read real/a.txt: %q %v", text, err)
	}
}

func TestFixLoopKeepsTheWorktreeGitFile(t *testing.T) {
	dir := gitRepo(t)
	if err := os.Symlink(".git", filepath.Join(dir, "cfg")); err != nil {
		t.Fatal(err)
	}
	if _, err := Git(context.Background(), dir, "add", "cfg"); err != nil {
		t.Fatal(err)
	}
	if _, err := Git(context.Background(), dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "link"); err != nil {
		t.Fatal(err)
	}
	var fixes atomic.Int64
	server := fakeModelsWriting(t, &fixes, "cfg", "gitdir: /tmp/evil\n")
	defer server.Close()
	fixer := overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "fixer", Concurrency: 1}
	var out bytes.Buffer
	result, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, Rounds: 1, Until: "high", Out: &out})
	if err != nil || fixes.Load() != 1 || !strings.Contains(out.String(), "made no changes") {
		t.Fatalf("fix through a link to .git: %+v %v\n%s", result, err, out.String())
	}
	assertNoFixBranches(t, dir)

	// Tests run model-written code that can write anything; the loop
	// notices a changed .git file, restores it and stops.
	server = fakeModels(t, &fixes)
	defer server.Close()
	fixer.BaseURL = server.URL
	out.Reset()
	result, err = Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, TestCommand: "printf 'gitdir: /tmp/evil' > .git", Rounds: 1, Until: "high", Out: &out})
	if err == nil || !strings.Contains(err.Error(), ".git file was changed") || result.Worktree == "" {
		t.Fatalf("tests that rewrote .git: %+v %v\n%s", result, err, out.String())
	}
	if data, err := os.ReadFile(filepath.Join(result.Worktree, ".git")); err != nil || !strings.HasPrefix(string(data), "gitdir: /") || !strings.Contains(string(data), "/.git/worktrees/") {
		t.Fatalf("the .git file was not restored: %q %v", data, err)
	}
	if head, err := Git(context.Background(), result.Worktree, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || strings.TrimSpace(head) != result.Branch {
		t.Fatalf("restored worktree HEAD = %q %v", head, err)
	}
}

func TestFixReportsAnInterruptDuringTheFinalTests(t *testing.T) {
	dir := gitRepo(t)
	var fixes atomic.Int64
	server := fakeModels(t, &fixes)
	defer server.Close()
	fixer := overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "fixer", Concurrency: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if started, _ := filepath.Glob(filepath.Join(dir, ".git", "overload-fix", "*", "started")); len(started) > 0 {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	result, err := Fix(ctx, FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, TestCommand: "touch started; sleep 30", Rounds: 1, Until: "high"})
	if !errors.Is(err, context.Canceled) || result.TestsPassed != nil {
		t.Fatalf("interrupted final tests: %+v %v", result, err)
	}
}

func TestGitFiltersNeverRun(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, dir, command string)
	}{
		{"clean filter from repository config, attributes on the base", func(t *testing.T, dir, command string) {
			gitConfig(t, dir, "filter.evil.clean", command)
			gitConfig(t, dir, "filter.evil.smudge", command)
			commitAttributes(t, dir, "main", "*.go filter=evil\n")
		}},
		{"process filter with a dotted name, attributes added by the branch", func(t *testing.T, dir, command string) {
			gitConfig(t, dir, "filter.evil.v2.process", command)
			gitConfig(t, dir, "filter.evil.v2.required", "true")
			commitAttributes(t, dir, "feature", "*.go filter=evil.v2\n")
		}},
		{"clean filter from the user's global config", func(t *testing.T, dir, command string) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[filter \"evil\"]\n\tclean = "+command+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			commitAttributes(t, dir, "feature", "*.go filter=evil\n")
		}},
		{"clean filter from inherited environment", func(t *testing.T, dir, command string) {
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "filter.evil.clean")
			t.Setenv("GIT_CONFIG_VALUE_0", command)
			commitAttributes(t, dir, "feature", "*.go filter=evil\n")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := gitRepo(t)
			marker := filepath.Join(t.TempDir(), "filter-ran")
			test.setup(t, dir, "touch "+marker+"; cat")
			// An uncommitted change makes git read the working-tree file.
			if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package app\n\nfunc ok() {}\n\nfunc broken() { bug() }\n\nfunc more() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var fixes atomic.Int64
			server := fakeModels(t, &fixes)
			defer server.Close()
			if _, err := Diff(context.Background(), dir, "main"); err != nil {
				t.Fatal(err)
			}
			if _, err := Review(context.Background(), dir, "main", workflowFor(server.URL)); err != nil {
				t.Fatal(err)
			}
			fixer := overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "fixer", Concurrency: 1}
			var out bytes.Buffer
			if _, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, Rounds: 1, Until: "high", Out: &out}); err != nil {
				t.Fatalf("fix: %v\n%s", err, out.String())
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a configured filter ran a program")
			}
		})
	}
}

func gitConfig(t *testing.T, dir, key, value string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", dir, "config", key, value).CombinedOutput(); err != nil {
		t.Fatalf("git config: %v %s", err, out)
	}
}

// commitAttributes commits a .gitattributes file on branch, with the test
// filters switched off, and leaves the feature branch checked out on top
// of it.
func commitAttributes(t *testing.T, dir, branch, attributes string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(attributes), 0o644); err != nil {
		t.Fatal(err)
	}
	steps := [][]string{{"stash", "-u", "-q"}, {"checkout", "-q", branch}, {"stash", "pop", "-q"}, {"add", ".gitattributes"}, {"commit", "-q", "-m", "attributes"}}
	if branch == "main" {
		steps = append(steps, []string{"rebase", "-q", "main", "feature"})
	}
	for _, args := range steps {
		command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "filter.evil.clean=", "-c", "filter.evil.smudge=", "-c", "filter.evil.process=", "-c", "filter.evil.v2.process=", "-c", "filter.evil.required=false", "-c", "filter.evil.v2.required=false"}, args...)...)
		command.Env = slices.DeleteFunc(os.Environ(), func(variable string) bool { return strings.HasPrefix(variable, "GIT_CONFIG_") })
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func TestDiffShowsIgnoredSubmodules(t *testing.T) {
	dir := gitRepo(t)
	head, err := Git(context.Background(), dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Git(context.Background(), dir, "update-index", "--add", "--cacheinfo", "160000,"+strings.TrimSpace(head)+",vendor/lib"); err != nil {
		t.Fatal(err)
	}
	if _, err := Git(context.Background(), dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "submodule"); err != nil {
		t.Fatal(err)
	}
	gitConfig(t, dir, "diff.ignoreSubmodules", "all")
	repo, err := openRepository(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := repo.diff(context.Background(), "main", true)
	if err != nil || !strings.Contains(diff, "+Subproject commit "+strings.TrimSpace(head)) {
		t.Fatalf("submodule update hidden: %q %v", diff, err)
	}
}

func TestFixCleansUpAFailedWorktreeAdd(t *testing.T) {
	dir := gitRepo(t)
	// A file where the worktree folder should go makes git fail after it
	// has created the branch.
	if err := os.WriteFile(filepath.Join(dir, ".git", "overload-fix"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var fixes atomic.Int64
	server := fakeModels(t, &fixes)
	defer server.Close()
	fixer := overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "fixer", Concurrency: 1}
	if _, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, Rounds: 1, Until: "high"}); err == nil || !strings.Contains(err.Error(), "worktree") {
		t.Fatalf("fix with a blocked worktree folder: %v", err)
	}
	if branches, err := Git(context.Background(), dir, "branch", "--list", "overload/fix-*"); err != nil || strings.TrimSpace(branches) != "" {
		t.Fatalf("branches left: %q %v", branches, err)
	}
	if worktrees, err := Git(context.Background(), dir, "worktree", "list", "--porcelain"); err != nil || strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("worktrees left: %q %v", worktrees, err)
	}
}

func TestFilterDrivers(t *testing.T) {
	output := "filter.lfs.clean\ngit-lfs clean -- %f\x00filter.lfs.required\ntrue\x00filter.a.b.process\nx\x00filter.evil\nno setting\x00"
	if got := strings.Join(filterDrivers(output), ","); got != "lfs,a.b" {
		t.Fatalf("filterDrivers = %s", got)
	}
}

func TestDirtySubmoduleFiltersNeverRun(t *testing.T) {
	dir := gitRepo(t)
	library := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "protocol.file.allow=always"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(library, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(library, "f"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(library, "add", "f")
	run(library, "commit", "-q", "-m", "library")
	run(dir, "submodule", "add", "-q", library, "lib")
	run(dir, "commit", "-q", "-m", "add library")
	marker := filepath.Join(t.TempDir(), "filter-ran")
	gitConfig(t, filepath.Join(dir, "lib"), "filter.subf.clean", "touch "+marker+"; cat")
	if err := os.WriteFile(filepath.Join(dir, "lib", ".gitattributes"), []byte("* filter=subf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "f"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plain git status in the parent would run the submodule's filter.
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the marker exists before the test")
	}
	var fixes atomic.Int64
	server := fakeModels(t, &fixes)
	defer server.Close()
	diff, err := Diff(context.Background(), dir, "main")
	if err != nil || !strings.Contains(diff, "+Subproject commit ") {
		t.Fatalf("diff = %q, %v", diff, err)
	}
	if _, err := Review(context.Background(), dir, "main", workflowFor(server.URL)); err != nil {
		t.Fatal(err)
	}
	fixer := overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "fixer", Concurrency: 1}
	var out bytes.Buffer
	if _, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, Rounds: 1, Until: "high", Out: &out}); err != nil {
		t.Fatalf("fix: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a submodule's own filter ran a program")
	}
	// The test is only meaningful if git would run the filter unprotected.
	if out, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); err != nil {
		t.Fatalf("git status: %v %s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("plain git status did not run the submodule's filter; the test proves nothing")
	}
}

func TestGlobalOptions(t *testing.T) {
	for _, test := range []struct {
		args       []string
		global     int
		subcommand string
	}{
		{[]string{"status"}, 0, "status"},
		{[]string{"--git-dir", "/repo/.git", "--work-tree", "/repo", "diff", "HEAD"}, 4, "diff"},
		{[]string{"--git-dir=/repo/.git", "--work-tree=/repo", "--literal-pathspecs", "add", "--", "a"}, 3, "add"},
		{[]string{"-c", "user.name=x", "-C", "/repo", "commit", "-m", "m"}, 4, "commit"},
		{[]string{"--namespace", "team", "--config-env", "core.editor=EDITOR", "--attr-source", "HEAD", "log"}, 6, "log"},
		{[]string{"--namespace=team", "--attr-source=HEAD", "--super-prefix", "sub/", "status"}, 4, "status"},
		{[]string{"status", "-c", "x"}, 0, "status"},
	} {
		global := globalOptions(test.args)
		if len(global) != test.global || !slices.Equal(global, test.args[:test.global]) || subcommand(test.args) != test.subcommand {
			t.Errorf("globalOptions(%q) = %q, subcommand %q", test.args, global, subcommand(test.args))
		}
	}
}

func TestGitWithSeparateRepositoryOptions(t *testing.T) {
	dir := gitRepo(t)
	marker := filepath.Join(t.TempDir(), "filter-ran")
	gitConfig(t, dir, "filter.evil.clean", "touch "+marker+"; cat")
	commitAttributes(t, dir, "feature", "*.go filter=evil\n")
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package app\n\nfunc changed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := Git(context.Background(), t.TempDir(), "--git-dir", filepath.Join(dir, ".git"), "--work-tree", dir, "diff", "--no-color", "HEAD", "--")
	if err != nil || !strings.Contains(diff, "+func changed() {}") {
		t.Fatalf("diff = %q, %v", diff, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a filter ran when the repository was named with separate options")
	}
}

func TestFixKeepsTheBranchWhenACommitErrorFollowsTheCommit(t *testing.T) {
	for _, test := range []struct {
		name   string
		commit func(context.Context, repository, string) error
		kept   bool
	}{
		{"committed then interrupted", func(ctx context.Context, worktree repository, message string) error {
			if err := commitRound(ctx, worktree, message); err != nil {
				return err
			}
			return context.Canceled
		}, true},
		{"failed before committing", func(context.Context, repository, string) error {
			return errors.New("disk full")
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := gitRepo(t)
			var fixes atomic.Int64
			server := fakeModels(t, &fixes)
			defer server.Close()
			fixer := overload.ModelProfile{Provider: "openaicompat", BaseURL: server.URL, Model: "fixer", Concurrency: 1}
			var out bytes.Buffer
			result, err := Fix(context.Background(), FixOptions{Dir: dir, Base: "main", Workflow: workflowFor(server.URL), Fixer: fixer, Rounds: 1, Until: "high", Out: &out, commit: test.commit})
			if err == nil {
				t.Fatalf("a failed commit was not reported\n%s", out.String())
			}
			if !test.kept {
				if result.Branch != "" {
					t.Fatalf("result = %+v", result)
				}
				assertNoFixBranches(t, dir)
				return
			}
			if result.Branch == "" || !strings.Contains(out.String(), "Kept branch") {
				t.Fatalf("result = %+v\n%s", result, out.String())
			}
			log, err := Git(context.Background(), dir, "log", "--format=%s", result.Branch)
			if err != nil || !strings.Contains(log, "overload: fix review findings (round 1)") {
				t.Fatalf("branch log: %q %v", log, err)
			}
			if _, err := os.Stat(result.Worktree); err != nil {
				t.Fatalf("worktree removed: %v", err)
			}
		})
	}
}
