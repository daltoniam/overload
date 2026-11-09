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
	for _, test := range []struct {
		name                 string
		input                GateInput
		event, state, phrase string
	}{
		{"clean comment", GateInput{Routing: routing("a.go")}, ReviewComment, GateSuccess, "No issues found"},
		{"minor findings", GateInput{Routing: routing("a.go"), Severities: []string{"low", "low"}}, ReviewComment, GateSuccess, "2 issue(s)"},
		{"blocking", GateInput{Decision: ReviewDecisionRequestChanges, Routing: routing("a.go"), Severities: []string{"critical"}}, ReviewRequestChanges, GateFailure, "Changes requested"},
		{"approve", GateInput{Decision: ReviewDecisionApprove, Routing: routing("a.go")}, ReviewApprove, GateSuccess, "Approved"},
		{"too large fails closed", GateInput{Decision: ReviewDecisionApprove, Routing: Routing{TooLarge: []string{"big.go"}, Agents: []AgentFiles{{Agent: "main", Files: []string{"a.go"}}}}, Degraded: true}, ReviewComment, GateFailure, "1 file(s) not reviewed"},
		{"failed agent fails closed", GateInput{Routing: Routing{Agents: []AgentFiles{{Agent: "main", Files: []string{"a.go", "b.go"}, Unreviewed: []string{"b.go"}}, {Agent: "sec", Files: []string{"b.go"}, Unreviewed: []string{"b.go"}}}}, Degraded: true}, ReviewComment, GateFailure, "1 file(s) not reviewed"},
		{"degraded without unreviewed files passes", GateInput{Routing: routing("a.go"), Degraded: true}, ReviewComment, GateSuccess, "Partial review. No issues found"},
		{"human paths", GateInput{Decision: ReviewDecisionApprove, HumanReviewPaths: []string{"auth/**"}, Routing: routing("auth/login.go", "a.go")}, ReviewComment, GateSuccess, "1 sensitive file(s)"},
		{"human paths still block", GateInput{Decision: ReviewDecisionApprove, HumanReviewPaths: []string{"auth/**"}, Routing: routing("auth/login.go"), Severities: []string{"high"}}, ReviewRequestChanges, GateFailure, "sensitive"},
		{"approve paths outside", GateInput{Decision: ReviewDecisionApprove, ApprovePaths: []string{"docs/**"}, Routing: routing("docs/a.md", "main.go")}, ReviewComment, GateSuccess, "No issues found"},
		{"approve paths inside", GateInput{Decision: ReviewDecisionApprove, ApprovePaths: []string{"docs/**"}, Routing: routing("docs/a.md")}, ReviewApprove, GateSuccess, "Approved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := GateOutcome(test.input)
			if gate.Event != test.event || gate.State != test.state || !strings.Contains(gate.Description, test.phrase) {
				t.Fatalf("gate = %+v", gate)
			}
		})
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
