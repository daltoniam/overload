package overload

import (
	"strings"
	"testing"
)

func TestReviewEvent(t *testing.T) {
	tests := []struct {
		name       string
		decision   string
		block      string
		partial    bool
		severities []string
		want       string
	}{
		{"comment mode ignores severity", "", "", false, []string{"critical"}, ReviewComment},
		{"explicit comment", ReviewDecisionComment, "", false, []string{"critical"}, ReviewComment},
		{"blocking finding requests changes", ReviewDecisionRequestChanges, "", false, []string{"low", "high"}, ReviewRequestChanges},
		{"below default threshold comments", ReviewDecisionRequestChanges, "", false, []string{"medium", "low"}, ReviewComment},
		{"custom threshold", ReviewDecisionRequestChanges, "medium", false, []string{"medium"}, ReviewComment + ""},
		{"request mode never approves", ReviewDecisionRequestChanges, "", false, nil, ReviewComment},
		{"approve when clean", ReviewDecisionApprove, "", false, nil, ReviewApprove},
		{"approve with minor findings", ReviewDecisionApprove, "", false, []string{"low"}, ReviewApprove},
		{"approve mode still blocks", ReviewDecisionApprove, "", false, []string{"critical"}, ReviewRequestChanges},
		{"partial review never approves", ReviewDecisionApprove, "", true, nil, ReviewComment},
		{"partial review still blocks", ReviewDecisionApprove, "", true, []string{"high"}, ReviewRequestChanges},
		{"unknown severity does not block", ReviewDecisionApprove, "low", false, []string{"weird"}, ReviewApprove},
	}
	tests[4].want = ReviewRequestChanges
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReviewEvent(tt.decision, tt.block, tt.partial, tt.severities); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestReviewDecisionValidation(t *testing.T) {
	ok := Workflow{Name: "r", Kind: "pr_review", Agents: []string{"a"}, ReviewDecision: ReviewDecisionApprove, BlockSeverity: "medium"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, workflow := range map[string]Workflow{
		"unknown decision":       {Name: "r", Kind: "pr_review", Agents: []string{"a"}, ReviewDecision: "merge"},
		"unknown severity":       {Name: "r", Kind: "pr_review", Agents: []string{"a"}, ReviewDecision: ReviewDecisionApprove, BlockSeverity: "severe"},
		"severity without block": {Name: "r", Kind: "pr_review", Agents: []string{"a"}, BlockSeverity: "high"},
		"scheduled workflow":     {Name: "r", Kind: "scheduled_prompt", Agents: []string{"a"}, ReviewDecision: ReviewDecisionApprove},
	} {
		if err := workflow.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGateOutcome(t *testing.T) {
	routing := func(files ...string) Routing {
		return Routing{Skipped: []string{"go.sum"}, Agents: []AgentFiles{{Agent: "main", Files: files}}}
	}
	changed := func(commentable []string, others ...string) *GateFiles {
		return &GateFiles{Changed: append(append([]string{}, commentable...), others...), Commentable: commentable}
	}
	reviewed := func(files ...string) (Routing, *GateFiles) {
		return routing(files...), changed(append([]string{"go.sum"}, files...))
	}
	gateInput := func(input GateInput, files ...string) GateInput {
		input.SkipPaths = []string{"go.sum"}
		input.Routing, input.Files = reviewed(files...)
		return input
	}
	for _, test := range []struct {
		name                 string
		input                GateInput
		event, state, phrase string
	}{
		{"clean comment", gateInput(GateInput{}, "a.go"), ReviewComment, GateSuccess, "No issues found"},
		{"minor findings", gateInput(GateInput{Severities: []string{"low", "low"}}, "a.go"), ReviewComment, GateSuccess, "2 issue(s)"},
		{"blocking", gateInput(GateInput{Decision: ReviewDecisionRequestChanges, Severities: []string{"critical"}}, "a.go"), ReviewRequestChanges, GateFailure, "Changes requested"},
		{"approve", gateInput(GateInput{Decision: ReviewDecisionApprove}, "a.go"), ReviewApprove, GateSuccess, "Approved"},
		{"approve not posted", gateInput(GateInput{Decision: ReviewDecisionApprove, NotPosted: true}, "a.go"), ReviewApprove, GateSuccess, "Would approve (not posted"},
		{"request changes not posted", gateInput(GateInput{Decision: ReviewDecisionApprove, Severities: []string{"high"}, NotPosted: true}, "a.go"), ReviewRequestChanges, GateFailure, "Would request changes"},
		{"too large fails closed", GateInput{Decision: ReviewDecisionApprove, Routing: Routing{TooLarge: []string{"big.go"}, Agents: []AgentFiles{{Agent: "main", Files: []string{"a.go"}}}}, Files: changed([]string{"a.go", "big.go"}), Degraded: true}, ReviewComment, GateFailure, "1 file(s) not reviewed"},
		{"failed agent fails closed", GateInput{Routing: Routing{Agents: []AgentFiles{{Agent: "main", Files: []string{"a.go", "b.go"}, Unreviewed: []string{"b.go"}}, {Agent: "sec", Files: []string{"b.go"}, Unreviewed: []string{"b.go"}}}}, Files: changed([]string{"a.go", "b.go"}), Degraded: true}, ReviewComment, GateFailure, "1 file(s) not reviewed"},
		{"missing routing fails closed", GateInput{Decision: ReviewDecisionApprove, Files: changed([]string{"a.go"})}, ReviewComment, GateFailure, "1 file(s) not reviewed"},
		{"unrouted file fails closed", GateInput{Decision: ReviewDecisionApprove, Routing: routing("a.go"), Files: changed([]string{"a.go", "b.go"})}, ReviewComment, GateFailure, "1 file(s) not reviewed"},
		{"unknown changed files fail closed", GateInput{Decision: ReviewDecisionApprove, Routing: routing("a.go")}, ReviewComment, GateFailure, "did not record its changed files"},
		{"a skip not on the skip paths fails closed", GateInput{Decision: ReviewDecisionApprove, Routing: Routing{Skipped: []string{"main.go"}, Agents: []AgentFiles{{Agent: "main", Files: []string{"a.go"}}}}, Files: changed([]string{"a.go", "main.go"})}, ReviewComment, GateFailure, "1 file(s) not reviewed"},
		{"degraded without unreviewed files passes", gateInput(GateInput{Degraded: true}, "a.go"), ReviewComment, GateSuccess, "Partial review. No issues found"},
		{"skipped files only never approve", gateInput(GateInput{Decision: ReviewDecisionApprove}), ReviewComment, GateSuccess, "No issues found"},
		{"human paths", gateInput(GateInput{Decision: ReviewDecisionApprove, HumanReviewPaths: []string{"auth/**"}}, "auth/login.go", "a.go"), ReviewComment, GateSuccess, "1 sensitive file(s)"},
		{"human paths still block", gateInput(GateInput{Decision: ReviewDecisionApprove, HumanReviewPaths: []string{"auth/**"}, Severities: []string{"high"}}, "auth/login.go"), ReviewRequestChanges, GateFailure, "sensitive"},
		{"human paths ignore case", gateInput(GateInput{Decision: ReviewDecisionApprove, HumanReviewPaths: []string{"auth/**"}}, "Auth/Login.go"), ReviewComment, GateSuccess, "1 sensitive file(s)"},
		{"a bare human path covers a folder", gateInput(GateInput{Decision: ReviewDecisionApprove, HumanReviewPaths: []string{"migrations"}}, "db/migrations/001.sql"), ReviewComment, GateSuccess, "1 sensitive file(s)"},
		{"a deleted human file blocks approval", GateInput{Decision: ReviewDecisionApprove, HumanReviewPaths: []string{"auth/**"}, Routing: routing("a.go"), Files: changed([]string{"a.go"}, "auth/old.go")}, ReviewComment, GateSuccess, "1 sensitive file(s)"},
		{"a file renamed out of a human path blocks approval", GateInput{Decision: ReviewDecisionApprove, HumanReviewPaths: []string{".github/**"}, Routing: routing("ci.yml"), Files: changed([]string{"ci.yml"}, ".github/workflows/ci.yml")}, ReviewComment, GateSuccess, "1 sensitive file(s)"},
		{"invalid human paths block approval", gateInput(GateInput{Decision: ReviewDecisionApprove, HumanReviewPaths: []string{"/etc"}}, "a.go"), ReviewComment, GateSuccess, "human review paths are invalid"},
		{"approve paths outside", gateInput(GateInput{Decision: ReviewDecisionApprove, ApprovePaths: []string{"docs/**", "go.sum"}}, "docs/a.md", "main.go"), ReviewComment, GateSuccess, "No issues found"},
		{"approve paths cover skipped files", gateInput(GateInput{Decision: ReviewDecisionApprove, ApprovePaths: []string{"docs/**"}}, "docs/a.md"), ReviewComment, GateSuccess, "No issues found"},
		{"approve paths cover binary files", GateInput{Decision: ReviewDecisionApprove, ApprovePaths: []string{"docs/**"}, Routing: routing("docs/a.md"), Files: changed([]string{"docs/a.md"}, "logo.png")}, ReviewComment, GateSuccess, "No issues found"},
		{"approve paths match case", gateInput(GateInput{Decision: ReviewDecisionApprove, ApprovePaths: []string{"docs/**", "go.sum"}}, "Docs/a.md"), ReviewComment, GateSuccess, "No issues found"},
		{"approve paths inside", gateInput(GateInput{Decision: ReviewDecisionApprove, ApprovePaths: []string{"docs/**", "go.sum"}}, "docs/a.md"), ReviewApprove, GateSuccess, "Approved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := GateOutcome(test.input)
			if gate.Event != test.event || gate.State != test.state || !strings.Contains(gate.Description, test.phrase) {
				t.Fatalf("gate = %+v", gate)
			}
		})
	}
}

func TestGateFilesFromDiff(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n" +
		"diff --git a/auth/old.go b/auth/old.go\ndeleted file mode 100644\n--- a/auth/old.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n" +
		"diff --git a/.github/ci.yml b/ci.yml\nsimilarity index 90%\nrename from .github/ci.yml\nrename to ci.yml\n--- a/.github/ci.yml\n+++ b/ci.yml\n@@ -1 +1 @@\n-x\n+y\n"
	files, err := GateFilesFromDiff(patch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files.Changed, ",") != "a.go,auth/old.go,.github/ci.yml,ci.yml" || strings.Join(files.Commentable, ",") != "a.go,ci.yml" {
		t.Fatalf("files %+v", files)
	}
}

func TestValidateGatePaths(t *testing.T) {
	workflow := Workflow{Name: "w", Kind: "pr_review", Agents: []string{"a"}, ReviewDecision: ReviewDecisionApprove, HumanReviewPaths: []string{"auth/**"}, ApprovePaths: []string{"docs/**"}}
	if err := workflow.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Workflow){
		"approve paths without approval": func(w *Workflow) { w.ReviewDecision = ReviewDecisionRequestChanges },
		"scheduled job":                  func(w *Workflow) { w.Kind, w.ReviewDecision, w.ApprovePaths = "scheduled_prompt", "", nil },
		"bad glob":                       func(w *Workflow) { w.HumanReviewPaths = []string{"/etc/passwd"} },
	} {
		invalid := workflow
		change(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
}
