package eval

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

func TestLabelCatches(t *testing.T) {
	label := Label{PR: 1, Path: "api/users.py", Line: 40, Window: 3, Keywords: []string{"Null", "missing check"}}
	for _, test := range []struct {
		name    string
		finding overload.Finding
		want    bool
	}{
		{"keyword in title within window", overload.Finding{Path: "api/users.py", Line: 43, Title: "Possible null dereference"}, true},
		{"keyword in body", overload.Finding{Path: "api/users.py", Line: 38, Body: "There is a missing check here"}, true},
		{"outside window", overload.Finding{Path: "api/users.py", Line: 44, Title: "null"}, false},
		{"other file", overload.Finding{Path: "api/user.py", Line: 40, Title: "null"}, false},
		{"no keyword", overload.Finding{Path: "api/users.py", Line: 40, Title: "Style"}, false},
	} {
		if got := label.Catches(test.finding); got != test.want {
			t.Errorf("%s: Catches = %v", test.name, got)
		}
	}
	if !(Label{Path: "a.go", Line: 5}).Catches(overload.Finding{Path: "a.go", Line: 5}) {
		t.Error("label without keywords should match by place")
	}
}

func TestBuildReportIsDeterministic(t *testing.T) {
	bug := func(path string, line int, title string) overload.Finding {
		return overload.Finding{Path: path, Line: line, Title: title, Severity: "high"}
	}
	runs := []RunResult{
		{PR: 102, Workflow: "sonnet", Status: "completed", Findings: []overload.Finding{bug("b.ts", 9, "Caller still uses removed field")}, CostUSD: 1.5, Seconds: 200},
		{PR: 101, Workflow: "sonnet", Status: "failed", Error: "model down", Findings: []overload.Finding{}, CostUSD: 0.1, Seconds: 30},
		{PR: 101, Workflow: "sol", Status: "completed", Findings: []overload.Finding{bug("a.py", 12, "Null user"), bug("a.py", 80, "Style")}, CostUSD: 0.8, Seconds: 120},
		{PR: 102, Workflow: "sol", Status: "completed", Findings: []overload.Finding{}, CostUSD: 0.4, Seconds: 90, Degraded: []string{"cost cap reached"}},
	}
	labels := []Label{
		{PR: 101, Path: "a.py", Line: 10, Window: 2, Keywords: []string{"null"}},
		{PR: 102, Path: "b.ts", Line: 9, Window: 0, Keywords: []string{"removed"}},
		{PR: 999, Path: "x.go", Line: 1},
	}
	report := BuildReport("acme/app", []int{101, 102}, []string{"sol", "sonnet"}, runs, labels)
	if len(report.Labels) != 2 || strings.Join(report.Labels[0].CaughtBy, ",") != "sol" || strings.Join(report.Labels[1].CaughtBy, ",") != "sonnet" {
		t.Fatalf("labels = %+v", report.Labels)
	}
	if report.Runs[0].PR != 101 || report.Runs[0].Workflow != "sol" || report.Runs[3].Workflow != "sonnet" {
		t.Fatalf("runs not ordered by PR then workflow: %+v", report.Runs)
	}
	sol, sonnet := report.Summary[0], report.Summary[1]
	if sol.Workflow != "sol" || sol.Runs != 2 || sol.Completed != 2 || sol.Degraded != 1 || sol.Caught != 1 || sol.Recall != 0.5 || sol.FindingsPerPR != 1 || math.Abs(sol.CostTotalUSD-1.2) > 1e-9 || sol.CostP50USD != 0.4 || sol.CostP90USD != 0.8 || sol.SecondsP90 != 120 {
		t.Fatalf("sol = %+v", sol)
	}
	if sonnet.Failed != 1 || sonnet.Caught != 1 || sonnet.Recall != 0.5 || sonnet.FindingsPerPR != 0.5 {
		t.Fatalf("sonnet = %+v", sonnet)
	}
	var csvOut bytes.Buffer
	if err := report.WriteCSV(&csvOut); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(csvOut.String()), "\n")
	if len(lines) != 3 || lines[1] != "sol,2,2,0,1,2,1,0.500,1.00,1.2000,0.4000,0.8000,90.0,120.0,0" {
		t.Fatalf("csv:\n%s", csvOut.String())
	}
}

func TestFailedRunsCatchNothing(t *testing.T) {
	runs := []RunResult{{PR: 1, Workflow: "w", Status: "failed", Findings: []overload.Finding{{Path: "a.go", Line: 3}}}}
	report := BuildReport("acme/app", []int{1}, []string{"w"}, runs, []Label{{PR: 1, Path: "a.go", Line: 3}})
	if report.Summary[0].Caught != 0 || len(report.Labels[0].CaughtBy) != 0 {
		t.Fatalf("failed run credited: %+v", report.Summary[0])
	}
}
