package eval

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing/fstest"
	"unicode"
	"unicode/utf8"
)

// A Case is one review to score: the changed files' contents, the diff a
// reviewer sees, and the bugs it should find.
type Case struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Files       []CaseFile        `json:"files"`
	Diff        string            `json:"diff"`
	Expected    []ExpectedFinding `json:"expected"`
}

// CaseFile is a file at the reviewed commit.
type CaseFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// A Suite is a named set of cases.
type Suite struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Cases       []Case `json:"cases"`
}

// Limits on suites people save.
const (
	MaxCases     = 50
	MaxCaseBytes = 512 << 10
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// Validate checks a suite before it is saved or run.
func (suite Suite) Validate() error {
	if !namePattern.MatchString(suite.Name) {
		return errors.New("invalid suite name")
	}
	if len(suite.Cases) == 0 || len(suite.Cases) > MaxCases {
		return fmt.Errorf("a suite needs 1 to %d cases", MaxCases)
	}
	seen := map[string]bool{}
	for _, item := range suite.Cases {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("case %s: %w", item.Name, err)
		}
		if seen[item.Name] {
			return fmt.Errorf("case %s appears twice", item.Name)
		}
		seen[item.Name] = true
	}
	return nil
}

// Validate checks a case.
func (item Case) Validate() error {
	if !namePattern.MatchString(item.Name) {
		return errors.New("invalid case name")
	}
	if len(item.Files) == 0 || strings.TrimSpace(item.Diff) == "" || len(item.Expected) == 0 {
		return errors.New("a case needs files, a diff and expected findings")
	}
	if strings.ContainsRune(item.Diff, 0) {
		return errors.New("the diff contains a NUL byte; cases hold text files only")
	}
	size := len(item.Diff)
	paths := map[string]bool{}
	for _, file := range item.Files {
		if !fs.ValidPath(file.Path) || file.Path == "." || paths[file.Path] {
			return fmt.Errorf("invalid or repeated file %q", file.Path)
		}
		if strings.ContainsRune(file.Content, 0) {
			return fmt.Errorf("file %q contains a NUL byte; cases hold text files only", file.Path)
		}
		paths[file.Path] = true
		size += len(file.Content)
	}
	// A file cannot also be a folder holding other files of the case.
	for name := range paths {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if paths[parent] {
				return fmt.Errorf("file %q is also a folder of %q", parent, name)
			}
		}
	}
	if size > MaxCaseBytes {
		return fmt.Errorf("a case may hold at most %d KiB of files and diff", MaxCaseBytes>>10)
	}
	lines := map[string]int{}
	for _, file := range item.Files {
		lines[file.Path] = lineCount(file.Content)
	}
	for _, expected := range item.Expected {
		if !paths[expected.Path] || expected.StartLine < 1 || expected.EndLine < expected.StartLine {
			return fmt.Errorf("expected finding %q must name a case file and a line range", expected.Description)
		}
		if expected.EndLine > lines[expected.Path] {
			return fmt.Errorf("expected finding %q ends at line %d, but %s has %d lines", expected.Description, expected.EndLine, expected.Path, lines[expected.Path])
		}
	}
	return nil
}

// lineCount is how many lines content has; a last line without a newline
// counts.
func lineCount(content string) int {
	count := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		count++
	}
	return count
}

// Repository returns the case's files as the repository a reviewer reads.
func (item Case) Repository() fs.FS {
	repo := fstest.MapFS{}
	for _, file := range item.Files {
		repo[file.Path] = &fstest.MapFile{Data: []byte(file.Content), Mode: 0o644}
	}
	return repo
}

// NewFileDiff is a diff that adds path with content, as if a pull request
// introduced the whole file.
func NewFileDiff(path, content string) string {
	var diff strings.Builder
	fmt.Fprintf(&diff, "diff --git a/%s b/%s\nnew file mode 100644\n", path, path)
	// Git writes no hunk for an empty file.
	if content == "" {
		return diff.String()
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	fmt.Fprintf(&diff, "--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, len(lines))
	for _, line := range lines {
		diff.WriteString("+" + line + "\n")
	}
	if !strings.HasSuffix(content, "\n") {
		diff.WriteString("\\ No newline at end of file\n")
	}
	return diff.String()
}

//go:embed fixtures/invoice.go.txt
var PlantedInvoice string

//go:embed fixtures/textutil.go.txt
var PlantedTextutil string

// PlantedSuite is the built-in suite: the planted-bug file the measurements
// in deploy/README.md use (five bugs), and a file with a misleading name
// that hides a SQL injection.
func PlantedSuite() Suite {
	invoice := Case{
		Name:        "planted-invoice",
		Description: "A 92-line Go file with five planted bugs",
		Files:       []CaseFile{{Path: "billing/invoice.go", Content: PlantedInvoice}},
		Diff:        NewFileDiff("billing/invoice.go", PlantedInvoice),
		Expected: []ExpectedFinding{
			{Path: "billing/invoice.go", StartLine: 23, EndLine: 28, Description: "mutex held on cache hit", Keywords: []string{"lock|deadlock|unlock|mutex"}},
			{Path: "billing/invoice.go", StartLine: 40, EndLine: 42, Description: "SQL injection", Keywords: []string{"sql|inject"}},
			{Path: "billing/invoice.go", StartLine: 59, EndLine: 65, Description: "pagination drops the last invoice", Keywords: []string{"last|off-by-one|off by one|drop|miss|page|paginat|slice|end"}},
			{Path: "billing/invoice.go", StartLine: 75, EndLine: 86, Description: "negative refund", Keywords: []string{"negative"}},
			{Path: "billing/invoice.go", StartLine: 70, EndLine: 91, Description: "stale cache after refund", Keywords: []string{"cache", "stale|invalidat|outdated|unchanged|old amount|old balance"}},
		},
	}
	textutil := Case{
		Name:        "planted-misleading-file",
		Description: "Database code in util/textutil.go with a SQL injection",
		Files:       []CaseFile{{Path: "util/textutil.go", Content: PlantedTextutil}},
		Diff:        NewFileDiff("util/textutil.go", PlantedTextutil),
		Expected: []ExpectedFinding{
			{Path: "util/textutil.go", StartLine: 14, EndLine: 16, Description: "SQL injection", Keywords: []string{"sql|inject"}},
		},
	}
	return Suite{Name: BuiltinSuite, Description: "Overload's planted-bug files", Cases: []Case{invoice, textutil}}
}

// BuiltinSuite names the built-in suite.
const BuiltinSuite = "planted"

// keywordsMatch reports whether text matches every keyword group; a group
// lists alternatives separated by |. A keyword must start a word, so "lock"
// does not match "block" nor "end" match "append".
func keywordsMatch(groups []string, text string) bool {
	text = strings.ToLower(text)
	for _, group := range groups {
		if !slices.ContainsFunc(strings.Split(group, "|"), func(word string) bool { return word != "" && startsWord(text, strings.ToLower(word)) }) {
			return false
		}
	}
	return true
}

// startsWord reports whether word occurs in text at the start of a word.
func startsWord(text, word string) bool {
	for offset := 0; offset <= len(text); {
		index := strings.Index(text[offset:], word)
		if index < 0 {
			return false
		}
		index += offset
		if before, _ := utf8.DecodeLastRuneInString(text[:index]); index == 0 || !isWordRune(before) {
			return true
		}
		offset = index + 1
	}
	return false
}

func isWordRune(char rune) bool {
	return char == '_' || unicode.IsLetter(char) || unicode.IsDigit(char) || unicode.IsMark(char)
}
