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
	// Bugs and findings are paired as a whole rather than in order: first
	// as many bugs as possible get a finding on their lines, then as many
	// of the rest as possible get a misplaced one. Pairing in order would
	// let a bug take the only finding another bug could match, so the
	// counts would depend on the order of the findings.
	describes := func(item ExpectedFinding, finding overload.Finding) bool {
		return finding.Path == item.Path && keywordsMatch(item.Keywords, finding.Title+" "+finding.Body)
	}
	// Without keywords any finding in the file would look like a misplaced
	// report of the bug, so only described bugs can be misplaced.
	hasKeywords := func(item ExpectedFinding) bool { return len(item.Keywords) > 0 }
	// A misplaced pair weighs less when its finding sits on another bug's
	// lines with that bug's words: left unpaired, it may still count as a
	// repeat of that bug rather than as another finding.
	onLineAnywhere := make([]bool, len(actual))
	for findingIndex, finding := range actual {
		for _, bug := range bugs {
			if slices.ContainsFunc(bug, func(item ExpectedFinding) bool { return describes(item, finding) && withinLines(item, finding) }) {
				onLineAnywhere[findingIndex] = true
			}
		}
	}
	misplacedWeight := len(actual) + 1
	onLines := (len(bugs) + 1) * misplacedWeight
	weights := make([][]int, len(bugs))
	for index, bug := range bugs {
		if slices.ContainsFunc(bug, hasKeywords) {
			result.PlacementChecked++
		}
		weights[index] = make([]int, len(actual))
		for findingIndex, finding := range actual {
			switch {
			case slices.ContainsFunc(bug, func(item ExpectedFinding) bool { return describes(item, finding) && withinLines(item, finding) }):
				weights[index][findingIndex] = onLines
			case slices.ContainsFunc(bug, func(item ExpectedFinding) bool { return hasKeywords(item) && describes(item, finding) }):
				weights[index][findingIndex] = misplacedWeight
				if onLineAnywhere[findingIndex] {
					weights[index][findingIndex]--
				}
			}
		}
	}
	for index, findingIndex := range maxWeightPairs(weights, len(actual)) {
		if findingIndex < 0 {
			continue
		}
		used[findingIndex] = true
		if weights[index][findingIndex] == onLines {
			matched[index] = true
			result.Matched++
		} else {
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

// maxWeightPairs pairs rows with columns to maximise the total weight
// (the Hungarian algorithm), returning each row's column or -1. A weight of
// zero means the pair is not allowed.
func maxWeightPairs(weights [][]int, columns int) []int {
	size := max(len(weights), columns)
	cost := func(row, column int) int {
		if row < len(weights) && column < columns {
			return -weights[row][column]
		}
		return 0
	}
	const infinity = int(^uint(0) >> 2)
	rowPotential, columnPotential := make([]int, size+1), make([]int, size+1)
	owner, way := make([]int, size+1), make([]int, size+1)
	for row := 1; row <= size; row++ {
		owner[0] = row
		column := 0
		lowest := make([]int, size+1)
		for index := range lowest {
			lowest[index] = infinity
		}
		visited := make([]bool, size+1)
		for {
			visited[column] = true
			current, delta, next := owner[column], infinity, 0
			for candidate := 1; candidate <= size; candidate++ {
				if visited[candidate] {
					continue
				}
				if reduced := cost(current-1, candidate-1) - rowPotential[current] - columnPotential[candidate]; reduced < lowest[candidate] {
					lowest[candidate], way[candidate] = reduced, column
				}
				if lowest[candidate] < delta {
					delta, next = lowest[candidate], candidate
				}
			}
			for candidate := 0; candidate <= size; candidate++ {
				if visited[candidate] {
					rowPotential[owner[candidate]] += delta
					columnPotential[candidate] -= delta
				} else {
					lowest[candidate] -= delta
				}
			}
			column = next
			if owner[column] == 0 {
				break
			}
		}
		for column != 0 {
			previous := way[column]
			owner[column] = owner[previous]
			column = previous
		}
	}
	pairs := make([]int, len(weights))
	for index := range pairs {
		pairs[index] = -1
	}
	for column := 1; column <= size; column++ {
		if row := owner[column] - 1; row >= 0 && row < len(weights) && column-1 < columns && weights[row][column-1] > 0 {
			pairs[row] = column - 1
		}
	}
	return pairs
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
