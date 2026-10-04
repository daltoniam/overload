package eval

import (
	"testing"

	"github.com/daltoniam/overload"
)

func TestScoreFindings(t *testing.T) {
	expected := []ExpectedFinding{{Path: "a.go", StartLine: 10, EndLine: 12}, {Path: "b.go", StartLine: 20, EndLine: 20}}
	findings := []overload.Finding{{Path: "a.go", Line: 13}, {Path: "a.go", Line: 11}, {Path: "c.go", Line: 20}}
	result := ScoreFindings(expected, findings, 1)
	if result.Matched != 1 || result.Unmatched != 2 || result.Recall != 0.5 {
		t.Fatalf("unexpected score: %+v", result)
	}
}
