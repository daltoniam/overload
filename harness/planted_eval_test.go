package harness

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/daltoniam/overload"
)

//go:embed testdata/planted/invoice.go.txt
var plantedInvoice string

type plantedBug struct {
	name  string
	match func(overload.Finding) bool
}

func findingText(finding overload.Finding) string {
	return strings.ToLower(finding.Title + " " + finding.Body)
}

var plantedBugs = []plantedBug{
	{"mutex held on cache hit", func(f overload.Finding) bool {
		text := findingText(f)
		return (f.Line >= 23 && f.Line <= 27) || strings.Contains(text, "deadlock") || strings.Contains(text, "unlock")
	}},
	{"SQL injection", func(f overload.Finding) bool {
		return f.Line >= 40 && f.Line <= 42 && strings.Contains(findingText(f), "sql")
	}},
	{"pagination drops last invoice", func(f overload.Finding) bool {
		return f.Line == 63 || (f.Line >= 59 && f.Line <= 64 && strings.Contains(findingText(f), "last"))
	}},
	{"negative refund", func(f overload.Finding) bool {
		return f.Line >= 75 && f.Line <= 86 && strings.Contains(findingText(f), "negative")
	}},
	{"stale cache after refund", func(f overload.Finding) bool {
		text := findingText(f)
		if !strings.Contains(text, "cache") {
			return false
		}
		for _, word := range []string{"stale", "invalidat", "outdated", "unchanged", "old amount", "old balance"} {
			if strings.Contains(text, word) {
				return true
			}
		}
		return false
	}},
}

// TestPlantedBugEval reviews a file with five planted bugs through the normal
// workflow path and reports which were found, tokens and time. Configure the
// model with OVERLOAD_EVAL_BASE_URL, OVERLOAD_EVAL_MODEL,
// OVERLOAD_EVAL_KIND (local or hosted), OVERLOAD_EVAL_KEY_ENV,
// OVERLOAD_EVAL_REASONING_PARAM, OVERLOAD_EVAL_REASONING_EFFORT,
// OVERLOAD_EVAL_MAX_OUTPUT_TOKENS and OVERLOAD_EVAL_RUNS.
func TestPlantedBugEval(t *testing.T) {
	if os.Getenv("OVERLOAD_PLANTED_EVAL") != "1" {
		t.Skip("set OVERLOAD_PLANTED_EVAL=1 for the live planted-bug eval")
	}
	baseURL, model := os.Getenv("OVERLOAD_EVAL_BASE_URL"), os.Getenv("OVERLOAD_EVAL_MODEL")
	if baseURL == "" || model == "" {
		t.Skip("OVERLOAD_EVAL_BASE_URL and OVERLOAD_EVAL_MODEL required")
	}
	runs, _ := strconv.Atoi(os.Getenv("OVERLOAD_EVAL_RUNS"))
	runs = max(runs, 1)
	maxTokens, _ := strconv.Atoi(os.Getenv("OVERLOAD_EVAL_MAX_OUTPUT_TOKENS"))
	prompt, _, err := loadPrompt("context")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(plantedInvoice, "\n"), "\n")
	var diff strings.Builder
	fmt.Fprintf(&diff, "diff --git a/billing/invoice.go b/billing/invoice.go\nnew file mode 100644\n--- /dev/null\n+++ b/billing/invoice.go\n@@ -0,0 +1,%d @@\n", len(lines))
	for _, line := range lines {
		diff.WriteString("+" + line + "\n")
	}
	workflow := overload.ResolvedWorkflow{Name: "planted-eval", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "eval", Model: overload.ModelProfile{Provider: "openaicompat", ConnectionKind: os.Getenv("OVERLOAD_EVAL_KIND"), BaseURL: baseURL, Model: model, APIKeyEnv: os.Getenv("OVERLOAD_EVAL_KEY_ENV"), Headers: evalHeaders(), ReasoningParam: os.Getenv("OVERLOAD_EVAL_REASONING_PARAM"), ReasoningEffort: os.Getenv("OVERLOAD_EVAL_REASONING_EFFORT"), MaxOutputTokens: maxTokens}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: prompt, SHA256: overload.PromptDigest(prompt)}}}}
	repo := fstest.MapFS{"billing/invoice.go": {Data: []byte(plantedInvoice)}}
	for run := 1; run <= runs; run++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
		start := time.Now()
		result, err := (Reviewer{}).Review(ctx, overload.ReviewSpec{Diff: diff.String(), Workflow: workflow}, repo)
		cancel()
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		found := 0
		var report []string
		for _, bug := range plantedBugs {
			hit := false
			for _, finding := range result.Findings {
				if bug.match(finding) {
					hit = true
					break
				}
			}
			if hit {
				found++
			}
			report = append(report, fmt.Sprintf("%s=%v", bug.name, hit))
		}
		t.Logf("run %d: %d/5 planted bugs, %d findings, %s, input=%v output=%v fallbacks=%v reasoning=%v", run, found, len(result.Findings), time.Since(start).Round(time.Second), result.Metrics["input_tokens"], result.Metrics["output_tokens"], result.Metrics["effort_fallbacks"], result.Metrics["agent_eval_reasoning"])
		t.Logf("run %d: %s", run, strings.Join(report, ", "))
		for _, finding := range result.Findings {
			t.Logf("  L%d [%s] %s: %s", finding.Line, finding.Severity, finding.Title, finding.Body)
		}
	}
}

func evalHeaders() map[string]string {
	if os.Getenv("OVERLOAD_EVAL_KIND") == "hosted" {
		return map[string]string{"cf-aig-skip-cache": "true"}
	}
	return nil
}

//go:embed testdata/planted/textutil.go.txt
var plantedTextutil string

func newFileDiff(path, content string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	var diff strings.Builder
	fmt.Fprintf(&diff, "diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, len(lines))
	for _, line := range lines {
		diff.WriteString("+" + line + "\n")
	}
	return diff.String()
}

// TestPlannerRoutingEval checks that the planner sends a file with a
// misleading name (database code in util/textutil.go) to the database
// sub-agent, whose paths do not match it, and that the planted SQL
// injection is found. It uses the same OVERLOAD_EVAL_* settings as
// TestPlantedBugEval for every agent.
func TestPlannerRoutingEval(t *testing.T) {
	if os.Getenv("OVERLOAD_PLANTED_EVAL") != "1" {
		t.Skip("set OVERLOAD_PLANTED_EVAL=1 for the live planted-bug eval")
	}
	baseURL, model := os.Getenv("OVERLOAD_EVAL_BASE_URL"), os.Getenv("OVERLOAD_EVAL_MODEL")
	if baseURL == "" || model == "" {
		t.Skip("OVERLOAD_EVAL_BASE_URL and OVERLOAD_EVAL_MODEL required")
	}
	maxTokens, _ := strconv.Atoi(os.Getenv("OVERLOAD_EVAL_MAX_OUTPUT_TOKENS"))
	profile := overload.ModelProfile{Provider: "openaicompat", ConnectionKind: os.Getenv("OVERLOAD_EVAL_KIND"), BaseURL: baseURL, Model: model, APIKeyEnv: os.Getenv("OVERLOAD_EVAL_KEY_ENV"), Headers: evalHeaders(), ReasoningParam: os.Getenv("OVERLOAD_EVAL_REASONING_PARAM"), ReasoningEffort: os.Getenv("OVERLOAD_EVAL_REASONING_EFFORT"), MaxOutputTokens: maxTokens}
	prompt := func(kind, body string) overload.PromptTemplate {
		return overload.PromptTemplate{Name: kind, Kind: kind, Body: body, Revision: 1, SHA256: overload.PromptDigest(body)}
	}
	entry, _, err := loadPrompt("context")
	if err != nil {
		t.Fatal(err)
	}
	planner := prompt(overload.PromptPlan, "Send any file that builds or runs SQL, or otherwise touches the database, to the database sub-agent.")
	workflow := overload.ResolvedWorkflow{Version: overload.SnapshotVersion, Name: "planner-eval", Kind: "pr_review", Revision: 1, MainReviews: overload.MainReviewsUnclaimed, PlannerPrompt: &planner, Agents: []overload.ResolvedAgent{
		{Name: "lead", Model: profile, EntryPrompt: prompt("entry", entry)},
		{Name: "database", Model: profile, EntryPrompt: prompt("entry", entry+"\n\nFocus on SQL injection, transactions and query correctness."), Scope: overload.Scope{Paths: []string{"**/store/**", "**/*.sql"}, Description: "SQL queries, database access and migrations"}},
	}}
	diff := newFileDiff("util/textutil.go", plantedTextutil) + newFileDiff("billing/invoice.go", plantedInvoice)
	repo := fstest.MapFS{"util/textutil.go": {Data: []byte(plantedTextutil)}, "billing/invoice.go": {Data: []byte(plantedInvoice)}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	start := time.Now()
	result, err := (Reviewer{}).Review(ctx, overload.ReviewSpec{Diff: diff, Workflow: workflow}, repo)
	if err != nil {
		t.Fatal(err)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	t.Logf("%s, planner: %s, input=%v output=%v", time.Since(start).Round(time.Second), routing.Planner, result.Metrics["input_tokens"], result.Metrics["output_tokens"])
	routed := false
	for _, path := range routing.Agents[1].Planned {
		routed = routed || path == "util/textutil.go"
	}
	found := false
	for _, finding := range result.Findings {
		t.Logf("  %s:%d [%s] %s (%s)", finding.Path, finding.Line, finding.Severity, finding.Title, strings.Join(finding.Agents, ", "))
		if finding.Path == "util/textutil.go" && finding.Line >= 14 && finding.Line <= 16 && strings.Contains(findingText(finding), "sql") {
			found = true
		}
	}
	t.Logf("misleading file routed to database: %v, SQL injection found: %v", routed, found)
	if !routed {
		t.Error("planner did not route util/textutil.go to the database sub-agent")
	}
}
