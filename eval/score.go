package eval

import (
	"slices"

	"github.com/daltoniam/overload"
)

// LineTolerance is how many lines away from the expected range a finding
// may cite and still count in bake-offs; models count lines from hunk
// headers and are often one or two off.
const LineTolerance = 2

type ExpectedFinding struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	Description string `json:"description"`
	// Keywords, when set, must all appear in the finding's title or body;
	// each lists alternatives separated by |, matched at the start of a word.
	Keywords []string `json:"keywords,omitempty"`
	// Bug groups expected findings that are one bug, such as the hunks of
	// one fix: the bug is found when a finding matches any of them, and
	// counts once. Findings without a bug name are each their own bug.
	Bug string `json:"bug,omitempty"`
}

type Score struct {
	// Expected counts bugs, so grouped expected findings count once.
	Expected int `json:"expected"`
	Matched  int `json:"matched"`
	Reported int `json:"reported"`
	// Unmatched counts findings that matched no expected bug, leaving out
	// the ones Misplaced counts.
	Unmatched int     `json:"unmatched"`
	Recall    float64 `json:"recall"`
	// Misplaced counts expected bugs a finding described (its keywords
	// matched in the right file) but cited outside the expected lines;
	// overload drops such findings before posting.
	Misplaced int `json:"misplaced"`
	// PlacementChecked counts expected bugs with keywords; only those can
	// be recognised when misplaced.
	PlacementChecked int `json:"placement_checked"`
	// Repeats counts further findings that describe a bug already found,
	// on its lines. It is only known when scoring; results store the
	// unmatched count that leaves repeats out.
	Repeats int `json:"repeats,omitempty"`
}

func ScoreFindings(expected []ExpectedFinding, actual []overload.Finding, tolerance int) Score {
	tolerance = max(tolerance, 0)
	bugs := groupBugs(expected)
	result := Score{Expected: len(bugs), Reported: len(actual)}
	used := make([]bool, len(actual))
	matched := make([]bool, len(bugs))
	withinLines := func(item ExpectedFinding, finding overload.Finding) bool {
		return item.StartLine >= 1 && item.EndLine >= item.StartLine && finding.Line >= item.StartLine-tolerance && finding.Line <= item.EndLine+tolerance
	}
	for index, bug := range bugs {
		matched[index] = claimFinding(bug, actual, used, withinLines)
		if matched[index] {
			result.Matched++
		}
	}
	hasKeywords := func(item ExpectedFinding) bool { return len(item.Keywords) > 0 }
	for index, bug := range bugs {
		if !slices.ContainsFunc(bug, hasKeywords) {
			continue
		}
		result.PlacementChecked++
		// Without keywords any finding in the file would look like a
		// misplaced report of the bug, so only described bugs count.
		if !matched[index] && claimFinding(bug, actual, used, func(item ExpectedFinding, _ overload.Finding) bool { return hasKeywords(item) }) {
			result.Misplaced++
		}
	}
	// A second finding on a bug already found (another hunk of the same
	// fix, or the bug reported twice) is a repeat, not another finding.
	for index, bug := range bugs {
		if !matched[index] {
			continue
		}
		for findingIndex, finding := range actual {
			if !used[findingIndex] && slices.ContainsFunc(bug, func(item ExpectedFinding) bool {
				return finding.Path == item.Path && withinLines(item, finding) && keywordsMatch(item.Keywords, finding.Title+" "+finding.Body)
			}) {
				used[findingIndex] = true
				result.Repeats++
			}
		}
	}
	result.Unmatched = result.Reported - result.Matched - result.Misplaced - result.Repeats
	if result.Expected > 0 {
		result.Recall = float64(result.Matched) / float64(result.Expected)
	}
	return result
}

// groupBugs returns the expected findings grouped by bug, in the order
// each bug first appears.
func groupBugs(expected []ExpectedFinding) [][]ExpectedFinding {
	var bugs [][]ExpectedFinding
	named := map[string]int{}
	for _, item := range expected {
		if item.Bug == "" {
			bugs = append(bugs, []ExpectedFinding{item})
			continue
		}
		if index, ok := named[item.Bug]; ok {
			bugs[index] = append(bugs[index], item)
			continue
		}
		named[item.Bug] = len(bugs)
		bugs = append(bugs, []ExpectedFinding{item})
	}
	return bugs
}

// claimFinding marks used the first unused finding in the right file whose
// text matches one of the bug's expected findings and that accept allows.
func claimFinding(bug []ExpectedFinding, actual []overload.Finding, used []bool, accept func(ExpectedFinding, overload.Finding) bool) bool {
	for _, item := range bug {
		for index, finding := range actual {
			if used[index] || finding.Path != item.Path || !keywordsMatch(item.Keywords, finding.Title+" "+finding.Body) || !accept(item, finding) {
				continue
			}
			used[index] = true
			return true
		}
	}
	return false
}
