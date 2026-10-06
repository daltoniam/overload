package review

import (
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

func TestCheckResultTreatsSandboxOutputAsUntrusted(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n-x\n+bad()\n+worse()\n"
	valid := overload.Finding{Path: "a.go", Line: 1, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Bug", Body: "Fix", Confidence: 0.9, Evidence: "bad()"}
	sneaky := valid
	sneaky.Line, sneaky.Evidence, sneaky.Title, sneaky.Body = 2, "worse()", "Ping @org/security", "See [this](https://evil.example)"
	offDiff := valid
	offDiff.Path = "other.go"
	withReason := valid
	withReason.DropReason = "verifier: <b>nit</b>"
	result := overload.ReviewResult{
		Findings: []overload.Finding{withReason},
		Dropped:  []overload.Finding{sneaky, offDiff},
		Metrics:  map[string]any{"routing": map[string]any{"agents": "not a list"}},
	}
	checked, err := CheckResult(result, patch, overload.DefaultMaxFindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(checked.Findings) != 1 || checked.Findings[0].DropReason != "" {
		t.Fatalf("kept findings must not carry a drop reason: %+v", checked.Findings)
	}
	if len(checked.Dropped) != 1 || checked.Dropped[0].DropReason != "dropped" {
		t.Fatalf("a dropped finding without a reason must still be stored as dropped: %+v", checked.Dropped)
	}
	if strings.Contains(checked.Dropped[0].Title, "@org") || strings.Contains(checked.Dropped[0].Body, "evil.example") {
		t.Fatalf("dropped finding text not sanitized: %+v", checked.Dropped[0])
	}
	if _, ok := checked.Metrics["routing"]; ok {
		t.Fatal("malformed routing kept")
	}
	good := overload.ReviewResult{Findings: []overload.Finding{valid}, Metrics: map[string]any{"routing": map[string]any{"agents": []any{map[string]any{"agent": "lead", "files": []any{"a.go"}}}}}}
	checked, err = CheckResult(good, patch, overload.DefaultMaxFindings)
	if routing, ok := checked.Metrics["routing"].(overload.Routing); err != nil || !ok || routing.Agents[0].Agent != "lead" {
		t.Fatalf("well-formed routing not decoded: %+v %v", checked.Metrics, err)
	}
}

func TestCheckResultKeepsAnEmptyFindingsList(t *testing.T) {
	checked, err := CheckResult(overload.ReviewResult{Findings: []overload.Finding{}}, "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n", overload.DefaultMaxFindings)
	if err != nil || checked.Findings == nil {
		t.Fatalf("findings must stay an empty list, not null: %#v %v", checked.Findings, err)
	}
}
