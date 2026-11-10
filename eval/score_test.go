package eval

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/daltoniam/overload"
)

func TestScoreFindings(t *testing.T) {
	described := []ExpectedFinding{{Path: "a.go", StartLine: 10, EndLine: 12, Keywords: []string{"lock|mutex"}}}
	oneFix := []ExpectedFinding{{Path: "a.go", StartLine: 10, EndLine: 10, Bug: CommitBug}, {Path: "a.go", StartLine: 40, EndLine: 41, Bug: CommitBug}}
	for _, test := range []struct {
		name     string
		expected []ExpectedFinding
		findings []overload.Finding
		want     Score
	}{
		{
			name:     "lines without keywords",
			expected: []ExpectedFinding{{Path: "a.go", StartLine: 10, EndLine: 12}, {Path: "b.go", StartLine: 20, EndLine: 20}},
			findings: []overload.Finding{{Path: "a.go", Line: 13}, {Path: "a.go", Line: 11}, {Path: "c.go", Line: 20}},
			want:     Score{Expected: 2, Matched: 1, Reported: 3, Unmatched: 1, Repeats: 1, Recall: 0.5},
		},
		{
			name:     "a misplaced finding is not also another finding",
			expected: described,
			findings: []overload.Finding{{Path: "a.go", Line: 30, Title: "Mutex held"}, {Path: "a.go", Line: 31, Title: "Unrelated"}},
			want:     Score{Expected: 1, Reported: 2, Unmatched: 1, Misplaced: 1, PlacementChecked: 1},
		},
		{
			name:     "keywords start a word",
			expected: described,
			findings: []overload.Finding{{Path: "a.go", Line: 11, Title: "Blocked request"}, {Path: "a.go", Line: 11, Body: "the lock is held"}},
			want:     Score{Expected: 1, Matched: 1, Reported: 2, Unmatched: 1, Recall: 1, PlacementChecked: 1},
		},
		{
			name:     "hunks of one fix are one bug",
			expected: oneFix,
			findings: []overload.Finding{{Path: "a.go", Line: 41}, {Path: "a.go", Line: 10}, {Path: "a.go", Line: 25}},
			want:     Score{Expected: 1, Matched: 1, Reported: 3, Unmatched: 1, Repeats: 1, Recall: 1},
		},
		{
			name:     "another bug on a found bug's lines is not a repeat",
			expected: described,
			findings: []overload.Finding{{Path: "a.go", Line: 11, Title: "Mutex held"}, {Path: "a.go", Line: 12, Title: "Ignored parse error"}, {Path: "a.go", Line: 10, Body: "the lock again"}},
			want:     Score{Expected: 1, Matched: 1, Reported: 3, Unmatched: 1, Repeats: 1, Recall: 1, PlacementChecked: 1},
		},
		{
			name:     "a grouped bug does not take another bug's only finding",
			expected: []ExpectedFinding{{Path: "a.go", StartLine: 10, EndLine: 10, Bug: "a"}, {Path: "a.go", StartLine: 30, EndLine: 30, Bug: "a"}, {Path: "a.go", StartLine: 11, EndLine: 11}},
			findings: []overload.Finding{{Path: "a.go", Line: 11}, {Path: "a.go", Line: 30}},
			want:     Score{Expected: 2, Matched: 2, Reported: 2, Recall: 1},
		},
		{
			name:     "misplaced findings are paired with bugs as a whole",
			expected: []ExpectedFinding{{Path: "a.go", StartLine: 10, EndLine: 10, Keywords: []string{"race"}}, {Path: "a.go", StartLine: 20, EndLine: 20, Keywords: []string{"race", "mutex"}}},
			findings: []overload.Finding{{Path: "a.go", Line: 50, Title: "race mutex"}, {Path: "a.go", Line: 60, Title: "race"}},
			want:     Score{Expected: 2, Reported: 2, Misplaced: 2, PlacementChecked: 2},
		},
		{
			name:     "on-line findings are chosen so a misplaced one still counts",
			expected: []ExpectedFinding{{Path: "a.go", StartLine: 10, EndLine: 10, Keywords: []string{"alpha"}}, {Path: "a.go", StartLine: 20, EndLine: 20, Keywords: []string{"beta"}}},
			findings: []overload.Finding{{Path: "a.go", Line: 10, Title: "alpha beta"}, {Path: "a.go", Line: 10, Title: "alpha"}},
			want:     Score{Expected: 2, Matched: 1, Reported: 2, Misplaced: 1, Recall: 0.5, PlacementChecked: 2},
		},
		{
			name:     "the same findings in the other order",
			expected: []ExpectedFinding{{Path: "a.go", StartLine: 10, EndLine: 10, Keywords: []string{"alpha"}}, {Path: "a.go", StartLine: 20, EndLine: 20, Keywords: []string{"beta"}}},
			findings: []overload.Finding{{Path: "a.go", Line: 10, Title: "alpha"}, {Path: "a.go", Line: 10, Title: "alpha beta"}},
			want:     Score{Expected: 2, Matched: 1, Reported: 2, Misplaced: 1, Recall: 0.5, PlacementChecked: 2},
		},
		{
			name:     "a missed fix is not checked for placement",
			expected: oneFix,
			findings: []overload.Finding{{Path: "a.go", Line: 25}},
			want:     Score{Expected: 1, Reported: 1, Unmatched: 1},
		},
	} {
		if got := ScoreFindings(test.expected, test.findings, 1); got != test.want {
			t.Errorf("%s: score = %+v, want %+v", test.name, got, test.want)
		}
	}
}

func TestKeywordsMatch(t *testing.T) {
	for _, test := range []struct {
		groups []string
		text   string
		want   bool
	}{
		{[]string{"lock"}, "Lock held on return", true},
		{[]string{"lock"}, "the call blocks", false},
		{[]string{"end"}, "append drops one", false},
		{[]string{"end"}, "the end index", true},
		{[]string{"sql|inject"}, "SQL-injection risk", true},
		{[]string{"cache", "stale|invalidat"}, "cache is never invalidated", true},
		{[]string{"cache", "stale|invalidat"}, "cache miss", false},
	} {
		if got := keywordsMatch(test.groups, test.text); got != test.want {
			t.Errorf("keywordsMatch(%q, %q) = %v", test.groups, test.text, got)
		}
	}
}

// TestScoreFindingsIgnoresOrder checks that shuffling the findings never
// changes the score.
func TestScoreFindingsIgnoresOrder(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	words := []string{"alpha", "beta", "gamma"}
	for trial := range 300 {
		var expected []ExpectedFinding
		for index := range 1 + random.IntN(4) {
			item := ExpectedFinding{Path: "a.go", StartLine: 10 * (index + 1), EndLine: 10*(index+1) + random.IntN(3)}
			if random.IntN(2) == 0 {
				item.Keywords = []string{words[random.IntN(len(words))]}
			}
			if random.IntN(3) == 0 {
				item.Bug = "shared"
			}
			expected = append(expected, item)
		}
		var findings []overload.Finding
		for range random.IntN(6) {
			title := words[random.IntN(len(words))] + " " + words[random.IntN(len(words))]
			findings = append(findings, overload.Finding{Path: "a.go", Line: 5 + random.IntN(45), Title: title})
		}
		want := ScoreFindings(expected, findings, 1)
		if want.Unmatched < 0 {
			t.Fatalf("trial %d: negative unmatched %+v", trial, want)
		}
		for range 5 {
			shuffled := slices.Clone(findings)
			random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if got := ScoreFindings(expected, shuffled, 1); got.Matched != want.Matched || got.Misplaced != want.Misplaced {
				t.Fatalf("trial %d: %+v then %+v for %+v and %+v", trial, want, got, expected, shuffled)
			}
		}
	}
}
