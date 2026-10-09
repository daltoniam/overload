package overload

import "testing"

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
