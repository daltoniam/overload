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
	// Keywords, when set, must all appear in the finding's title or body;
	// each lists alternatives separated by |.
	Keywords []string `json:"keywords,omitempty"`
}

type Score struct {
	Expected  int     `json:"expected"`
	Matched   int     `json:"matched"`
	Reported  int     `json:"reported"`
	Unmatched int     `json:"unmatched"`
	Recall    float64 `json:"recall"`
	// Misplaced counts expected bugs a finding described (its keywords
	// matched in the right file) but cited outside the expected lines;
	// overload drops such findings before posting.
	Misplaced int `json:"misplaced"`
}

func ScoreFindings(expected []ExpectedFinding, actual []overload.Finding, tolerance int) Score {
	if tolerance < 0 {
		tolerance = 0
	}
	result := Score{Expected: len(expected), Reported: len(actual)}
	used := make([]bool, len(actual))
	matched := make([]bool, len(expected))
	for expectedIndex, item := range expected {
		for index, finding := range actual {
			if used[index] || finding.Path != item.Path || !keywordsMatch(item.Keywords, finding.Title+" "+finding.Body) {
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
			matched[expectedIndex] = true
			result.Matched++
			break
		}
	}
	for index, item := range expected {
		if matched[index] || len(item.Keywords) == 0 {
			continue
		}
		for findingIndex, finding := range actual {
			if !used[findingIndex] && finding.Path == item.Path && keywordsMatch(item.Keywords, finding.Title+" "+finding.Body) {
				result.Misplaced++
				break
			}
		}
	}
	result.Unmatched = result.Reported - result.Matched
	if result.Expected > 0 {
		result.Recall = float64(result.Matched) / float64(result.Expected)
	}
	return result
}
