package eval

import (
	"github.com/daltoniam/overload"
)

type ExpectedFinding struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
}

type Score struct {
	Expected  int     `json:"expected"`
	Matched   int     `json:"matched"`
	Reported  int     `json:"reported"`
	Unmatched int     `json:"unmatched"`
	Recall    float64 `json:"recall"`
}

func ScoreFindings(expected []ExpectedFinding, actual []overload.Finding, tolerance int) Score {
	if tolerance < 0 {
		tolerance = 0
	}
	result := Score{Expected: len(expected), Reported: len(actual)}
	used := make([]bool, len(actual))
	for _, item := range expected {
		for index, finding := range actual {
			if used[index] || finding.Path != item.Path {
				continue
			}
			start := item.StartLine
			end := item.EndLine
			if start < 1 || end < start {
				continue
			}
			if finding.Line < start-tolerance || finding.Line > end+tolerance {
				continue
			}
			used[index] = true
			result.Matched++
			break
		}
	}
	result.Unmatched = result.Reported - result.Matched
	if result.Expected > 0 {
		result.Recall = float64(result.Matched) / float64(result.Expected)
	}
	return result
}
