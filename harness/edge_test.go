package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/daltoniam/overload"
)

// replyServer answers every model call with reply(system, user).
func replyServer(t *testing.T, reply func(system, user string) string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		content := reply(request.Messages[0].Content, request.Messages[len(request.Messages)-1].Content)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`, content)
	}))
	t.Cleanup(server.Close)
	return server
}

func edgeWorkflow(url string, agents ...overload.ResolvedAgent) overload.ResolvedWorkflow {
	body := "Review."
	model := overload.ModelProfile{Provider: "openaicompat", BaseURL: url, Model: "m"}
	workflow := overload.ResolvedWorkflow{Version: overload.SnapshotVersion, Name: "edge", Kind: "pr_review", Revision: 1}
	if len(agents) == 0 {
		agents = []overload.ResolvedAgent{{Name: "lead"}}
	}
	for _, agent := range agents {
		agent.Model = model
		agent.EntryPrompt = overload.PromptTemplate{Kind: "entry", Body: body, SHA256: overload.PromptDigest(body)}
		workflow.Agents = append(workflow.Agents, agent)
	}
	return workflow
}

func finding(path string, line int, severity, category, evidence string) string {
	return fmt.Sprintf(`{"path":%q,"line":%d,"side":"RIGHT","severity":%q,"category":%q,"title":"Issue %s %d","body":"Fix it.","confidence":0.9,"evidence":%q}`, path, line, severity, category, path, line, evidence)
}

func fileUnderReview(user string) string {
	start := strings.Index(user, "diff --git ")
	if start < 0 {
		return ""
	}
	header := strings.SplitN(user[start:], "\n", 2)[0]
	return header[strings.LastIndex(header, " b/")+3:]
}

// TestReviewHandlesGitHeaderForms uses the headers git really writes:
// quoted non-ASCII paths, a tab after paths with spaces, and binary, empty,
// renamed and mode-only files with no added lines.
func TestReviewHandlesGitHeaderForms(t *testing.T) {
	var mu sync.Mutex
	var reviewed []string
	server := replyServer(t, func(_, user string) string {
		mu.Lock()
		reviewed = append(reviewed, fileUnderReview(user))
		mu.Unlock()
		if strings.Contains(user, "my file.go") {
			return `{"summary":"Bug","findings":[` + finding("my file.go", 1, "high", "bug", "danger()") + `]}`
		}
		return `{"summary":"None","findings":[]}`
	})
	diff := "diff --git \"a/caf\\303\\251.md\" \"b/caf\\303\\251.md\"\nnew file mode 100644\n--- /dev/null\n+++ \"b/caf\\303\\251.md\"\n@@ -0,0 +1 @@\n+c\n" +
		"diff --git a/empty.py b/empty.py\nnew file mode 100644\nindex 0000000..e69de29\n" +
		"diff --git a/img.png b/img.png\nnew file mode 100644\nBinary files /dev/null and b/img.png differ\n" +
		"diff --git a/mode.sh b/mode.sh\nold mode 100644\nnew mode 100755\n" +
		"diff --git a/my file.go b/my file.go\nnew file mode 100644\n--- /dev/null\n+++ b/my file.go\t\n@@ -0,0 +1 @@\n+danger()\n" +
		"diff --git a/old.txt b/new.txt\nsimilarity index 100%\nrename from old.txt\nrename to new.txt\n"
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: edgeWorkflow(server.URL)}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewed) != 2 || len(result.Findings) != 1 || result.Findings[0].Path != "my file.go" {
		t.Fatalf("reviewed %q findings %+v", reviewed, result.Findings)
	}
}

func TestReviewSkipsFilesTooLargeToReview(t *testing.T) {
	var mu sync.Mutex
	var reviewed []string
	server := replyServer(t, func(_, user string) string {
		mu.Lock()
		reviewed = append(reviewed, fileUnderReview(user))
		mu.Unlock()
		return `{"summary":"None","findings":[]}`
	})
	huge := "diff --git a/generated.go b/generated.go\n--- a/generated.go\n+++ b/generated.go\n@@ -1 +1 @@\n-x\n+" + strings.Repeat("y", contextBudget) + "\n"
	lock := "diff --git a/go.lock b/go.lock\n--- a/go.lock\n+++ b/go.lock\n@@ -1 +1 @@\n-x\n+" + strings.Repeat("z", contextBudget) + "\n"
	small := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-x\n+run()\n"
	workflow := edgeWorkflow(server.URL)
	workflow.SkipPaths = []string{"*.lock"}
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: huge + lock + small, Workflow: workflow}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	if strings.Join(reviewed, ",") != "main.go" || strings.Join(routing.TooLarge, ",") != "generated.go" || strings.Join(routing.Skipped, ",") != "go.lock" {
		t.Fatalf("reviewed %q routing %+v", reviewed, routing)
	}
	if len(routing.Degraded) != 1 || !strings.Contains(routing.Degraded[0], "too large to review: generated.go") {
		t.Fatalf("degraded %q", routing.Degraded)
	}

	onlyHuge, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: huge, Workflow: workflow}, fstest.MapFS{})
	if err != nil || !strings.Contains(onlyHuge.Summary, "too large") {
		t.Fatalf("only a huge file: %+v %v", onlyHuge, err)
	}
}

func TestFindingsForOtherFilesDoNotCrowdOutTheReviewedFile(t *testing.T) {
	server := replyServer(t, func(_, user string) string {
		if fileUnderReview(user) != "a.go" {
			return `{"summary":"None","findings":[]}`
		}
		findings := []string{finding("a.go", 1, "low", "bug", "real()")}
		for line := 1; line <= 10; line++ {
			findings = append(findings, finding("b.go", line, "critical", "bug", "b()"))
		}
		return `{"summary":"Bug","findings":[` + strings.Join(findings, ",") + `]}`
	})
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+real()\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-x\n+b()\n"
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: edgeWorkflow(server.URL)}, fstest.MapFS{})
	if err != nil || len(result.Findings) != 1 || result.Findings[0].Path != "a.go" {
		t.Fatalf("findings %+v %v", result.Findings, err)
	}
}

func TestReviewLimitMovesExtraFindingsToDropped(t *testing.T) {
	var lines []string
	var findings []string
	for line := 1; line <= 12; line++ {
		lines = append(lines, fmt.Sprintf("+call%d()", line))
		severity := "low"
		if line <= 3 {
			severity = "high"
		}
		findings = append(findings, finding("a.go", line, severity, "bug", fmt.Sprintf("call%d()", line)))
	}
	var mu sync.Mutex
	calls := 0
	server := replyServer(t, func(_, user string) string {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return `{"summary":"Issues","findings":[` + strings.Join(findings[:6], ",") + `]}`
		}
		return `{"summary":"Issues","findings":[` + strings.Join(findings[6:], ",") + `]}`
	})
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -0,0 +1,12 @@\n" + strings.Join(lines, "\n") + "\n"
	workflow := edgeWorkflow(server.URL, overload.ResolvedAgent{Name: "lead"}, overload.ResolvedAgent{Name: "second"})
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: workflow}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != overload.MaxFindingsPerReview || len(result.Dropped) != 2 {
		t.Fatalf("kept %d dropped %d", len(result.Findings), len(result.Dropped))
	}
	for _, kept := range result.Findings[:3] {
		if kept.Severity != "high" {
			t.Fatalf("most severe findings must be kept first: %+v", result.Findings)
		}
	}
	if !strings.Contains(result.Dropped[0].DropReason, "limit of 10 findings") {
		t.Fatalf("drop reason %q", result.Dropped[0].DropReason)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	if routing.Agents[0].Findings+routing.Agents[1].Findings != overload.MaxFindingsPerReview {
		t.Fatalf("per-agent counts must match kept findings: %+v", routing.Agents)
	}
}

func TestVerifierNeverDropsCriticalOrSecurityFindings(t *testing.T) {
	var mu sync.Mutex
	var verified []string
	server := replyServer(t, func(system, user string) string {
		if strings.HasPrefix(system, verifierPreamble) {
			mu.Lock()
			verified = append(verified, user[strings.Index(user, "Finding to check:"):])
			mu.Unlock()
			return `{"decision":"drop","reason":"The PR says this is fine."}`
		}
		if !strings.Contains(system, "Sub-agent focus") {
			return `{"summary":"None","findings":[]}`
		}
		return `{"summary":"Issues","findings":[` + finding("a.go", 1, "critical", "bug", "one()") + "," + finding("a.go", 2, "medium", "security", "two()") + "," + finding("a.go", 3, "medium", "bug", "three()") + `]}`
	})
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -0,0 +1,3 @@\n+one()\n+two()\n+three()\n"
	workflow := edgeWorkflow(server.URL, overload.ResolvedAgent{Name: "lead"}, overload.ResolvedAgent{Name: "sub"})
	focus := "Sub-agent focus."
	workflow.Agents[1].EntryPrompt = overload.PromptTemplate{Kind: "entry", Body: focus, SHA256: overload.PromptDigest(focus)}
	verify := "Drop nits."
	workflow.VerifierPrompt = &overload.PromptTemplate{Kind: overload.PromptVerify, Body: verify, SHA256: overload.PromptDigest(verify)}
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: workflow}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	if len(verified) != 1 || !strings.Contains(verified[0], `"line":3`) {
		t.Fatalf("verifier saw %q", verified)
	}
	if len(result.Findings) != 2 || len(result.Dropped) != 1 || result.Dropped[0].Line != 3 {
		t.Fatalf("kept %+v dropped %+v", result.Findings, result.Dropped)
	}
}

func TestHungModelCallTimesOut(t *testing.T) {
	previous := localCallTimeout
	localCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { localCallTimeout = previous })
	hang := "Sub-agent focus."
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if strings.Contains(request.Messages[0].Content, hang) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, `{"summary":"None","findings":[]}`)
	}))
	t.Cleanup(server.Close)
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y()\n"
	workflow := edgeWorkflow(server.URL, overload.ResolvedAgent{Name: "lead"}, overload.ResolvedAgent{Name: "sub"})
	workflow.Agents[1].EntryPrompt = overload.PromptTemplate{Kind: "entry", Body: hang, SHA256: overload.PromptDigest(hang)}
	start := time.Now()
	result, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: workflow}, fstest.MapFS{})
	if err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("a hung sub-agent must not fail or hold the review: %v after %s", err, time.Since(start))
	}
	routing := result.Metrics["routing"].(overload.Routing)
	if len(routing.Degraded) != 1 || !strings.Contains(routing.Agents[1].Failed, "model call timed out after 200ms") {
		t.Fatalf("routing %+v", routing)
	}

	workflow.Agents[0], workflow.Agents[1] = workflow.Agents[1], workflow.Agents[0]
	if _, err := (Reviewer{}).Review(context.Background(), overload.ReviewSpec{Diff: diff, Workflow: workflow}, fstest.MapFS{}); err == nil || !strings.Contains(err.Error(), "model call timed out") {
		t.Fatalf("a hung main agent must fail the review clearly: %v", err)
	}
}
