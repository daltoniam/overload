package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

func TestPlantedSuite(t *testing.T) {
	suite := PlantedSuite()
	if err := suite.Validate(); err != nil {
		t.Fatal(err)
	}
	invoice := suite.Cases[0]
	found := []overload.Finding{
		{Path: "billing/invoice.go", Line: 41, Title: "SQL injection", Body: "customer is concatenated into SQL"},
		{Path: "billing/invoice.go", Line: 26, Title: "Deadlock", Body: "returns without unlocking the mutex"},
		{Path: "billing/invoice.go", Line: 45, Title: "Off-by-one", Body: "drops the last invoice of each page"},
		{Path: "billing/invoice.go", Line: 88, Title: "Ignored parse error", Body: "amount parse error is ignored"},
	}
	score := ScoreFindings(invoice.Expected, found, 2)
	if score.Expected != 5 || score.Matched != 2 || score.Misplaced != 1 || score.Unmatched != 1 || score.PlacementChecked != 5 {
		t.Fatalf("score = %+v", score)
	}
	if _, err := invoice.Repository().Open("billing/invoice.go"); err != nil {
		t.Fatal(err)
	}
}

func TestSuiteValidate(t *testing.T) {
	good := PlantedSuite().Cases[1]
	for name, suite := range map[string]Suite{
		"no cases":        {Name: "s"},
		"bad name":        {Name: "a b", Cases: []Case{good}},
		"repeated case":   {Name: "s", Cases: []Case{good, good}},
		"unknown file":    {Name: "s", Cases: []Case{{Name: "c", Files: good.Files, Diff: good.Diff, Expected: []ExpectedFinding{{Path: "nope.go", StartLine: 1, EndLine: 1}}}}},
		"backwards":       {Name: "s", Cases: []Case{{Name: "c", Files: good.Files, Diff: good.Diff, Expected: []ExpectedFinding{{Path: "util/textutil.go", StartLine: 5, EndLine: 4}}}}},
		"escaping path":   {Name: "s", Cases: []Case{{Name: "c", Files: []CaseFile{{Path: "../x.go"}}, Diff: good.Diff, Expected: good.Expected}}},
		"too large":       {Name: "s", Cases: []Case{{Name: "c", Files: []CaseFile{{Path: "util/textutil.go", Content: strings.Repeat("x", MaxCaseBytes)}}, Diff: good.Diff, Expected: good.Expected}}},
		"missing diff":    {Name: "s", Cases: []Case{{Name: "c", Files: good.Files, Expected: good.Expected}}},
		"root as a file":  {Name: "s", Cases: []Case{{Name: "c", Files: []CaseFile{{Path: "."}, good.Files[0]}, Diff: good.Diff, Expected: good.Expected}}},
		"file and folder": {Name: "s", Cases: []Case{{Name: "c", Files: []CaseFile{{Path: "util"}, {Path: "util/textutil.go"}}, Diff: good.Diff, Expected: good.Expected}}},
		"NUL in a file":   {Name: "s", Cases: []Case{{Name: "c", Files: []CaseFile{{Path: "util/textutil.go", Content: "a\x00b"}}, Diff: good.Diff, Expected: good.Expected}}},
		"NUL in diff":     {Name: "s", Cases: []Case{{Name: "c", Files: good.Files, Diff: good.Diff + "\x00", Expected: good.Expected}}},
	} {
		if err := suite.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
}

// gitRepo makes a repository whose local configuration sets every option
// that changes git's output, so CaseFromCommit has to override them.
func gitRepo(t *testing.T) (string, func(...string) string, func(string, string)) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	for _, setting := range [][2]string{
		{"color.ui", "always"},
		{"color.diff", "always"},
		{"diff.noprefix", "true"},
		{"diff.mnemonicPrefix", "true"},
		{"diff.relative", "true"},
		{"diff.renames", "copies"},
		{"diff.context", "9"},
		{"diff.interHunkContext", "20"},
		{"diff.external", "echo external"},
		{"diff.shout.textconv", "tr a-z A-Z"},
		{"log.showSignature", "true"},
	} {
		git("config", setting[0], setting[1])
	}
	write(".gitattributes", "*.go diff=shout\n")
	return dir, git, write
}

func TestCaseFromCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const sum = "package calc\n\n// Sum adds the values.\nfunc Sum(values []int) int {\n\ttotal := 0\n\tfor i := 1; i < len(values); i++ {\n\t\ttotal += values[i]\n\t}\n\treturn total\n}\n"
	for _, test := range []struct {
		name   string
		before string
		after  string
		want   [][2]int
	}{
		{"changed line", sum, strings.Replace(sum, "i := 1", "i := 0", 1), [][2]int{{6, 6}}},
		{"pure insertion", "a\nb\nc\nd\n", "a\nb\nguard\nc\nd\n", [][2]int{{2, 3}}},
		{"insertion at the top", "a\nb\n", "check\na\nb\n", [][2]int{{1, 2}}},
		{"pure deletion", "a\nb\nbad\nc\n", "a\nb\nc\n", [][2]int{{3, 3}}},
		{"two hunks", "a\nbad\nc\nd\ne\nf\ng\nh\ni\nworse\n", "a\ngood\nc\nd\ne\nf\ng\nh\ni\nbetter\n", [][2]int{{2, 2}, {10, 10}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, git, write := gitRepo(t)
			write("calc/sum.go", test.before)
			write("README.md", "calc\n")
			write("logo.png", "\x89PNG\x00one")
			git("add", ".")
			git("commit", "-q", "-m", "add sum")
			write("calc/sum.go", test.after)
			write("logo.png", "\x89PNG\x00two")
			write("NEW.md", "added\n")
			git("add", ".")
			git("commit", "-q", "-m", "Fix Sum skipping the first value")
			fix := git("rev-parse", "HEAD")

			// A subdirectory checks that paths stay relative to the top.
			item, err := CaseFromCommit(context.Background(), filepath.Join(dir, "calc"), fix[:10], "")
			if err != nil {
				t.Fatal(err)
			}
			if item.Name != "commit-"+fix[:12] || item.Description != "Fix Sum skipping the first value" || len(item.Files) != 1 || item.Files[0].Path != "calc/sum.go" || item.Files[0].Content != test.before {
				t.Fatalf("case = %+v", item)
			}
			if !strings.HasPrefix(item.Diff, "diff --git a/calc/sum.go b/calc/sum.go\n") || strings.Contains(item.Diff, "\x1b") || strings.Contains(item.Diff, "external") {
				t.Fatalf("the diff depends on git configuration:\n%s", item.Diff)
			}
			var lines [][2]int
			for _, expected := range item.Expected {
				if expected.Bug != CommitBug {
					t.Errorf("expected finding %+v is not part of the fix", expected)
				}
				lines = append(lines, [2]int{expected.StartLine, expected.EndLine})
			}
			if !slices.Equal(lines, test.want) {
				t.Fatalf("expected lines = %v, want %v", lines, test.want)
			}
			last := test.want[len(test.want)-1]
			if score := ScoreFindings(item.Expected, []overload.Finding{{Path: "calc/sum.go", Line: last[0]}}, 0); score.Matched != 1 || score.Expected != 1 {
				t.Fatalf("score = %+v", score)
			}
		})
	}
}

func TestCaseFromCommitRefuses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, git, write := gitRepo(t)
	write("a.go", "package a\n")
	write("logo.png", "\x89PNG\x00one")
	git("add", ".")
	git("commit", "-q", "-m", "first")
	root := git("rev-parse", "HEAD")
	write("logo.png", "\x89PNG\x00two")
	git("commit", "-q", "-am", "binary only")
	binary := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "side")
	write("a.go", "package a\n\nconst A = 1\n")
	git("commit", "-q", "-am", "side")
	git("checkout", "-q", "main")
	write("b.go", "package a\n")
	git("add", ".")
	git("commit", "-q", "-m", "main")
	git("merge", "-q", "--no-ff", "--no-edit", "side")
	merge := git("rev-parse", "HEAD")
	// A submodule pointer change looks like a modified text file.
	git("update-index", "--add", "--cacheinfo", "160000,"+root+",vendor/lib")
	git("commit", "-q", "-m", "add submodule")
	git("update-index", "--cacheinfo", "160000,"+binary+",vendor/lib")
	git("commit", "-q", "-m", "bump submodule")
	submodule := git("rev-parse", "HEAD")
	for _, test := range []struct{ commit, want string }{
		{"", "as a hash"},
		{"HEAD", "as a hash"},
		{"--upload-pack=x", "as a hash"},
		{"zzzzzzz", "as a hash"},
		{root, "no parent"},
		{binary, "no existing text files"},
		{merge, "is a merge"},
		{submodule, "no existing text files"},
	} {
		if _, err := CaseFromCommit(context.Background(), dir, test.commit, ""); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q: error %v, want %q", test.commit, err, test.want)
		}
	}
}

func TestCaseFromCommitSkipsSubmodulesWithoutTheirCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, git, write := gitRepo(t)
	write("a.go", "package a\n\nconst A = 1\n")
	git("add", ".")
	// The submodule's commits live in its own repository, so these hashes
	// name objects this repository does not have.
	git("update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,vendor/lib")
	git("commit", "-q", "-m", "first")
	write("a.go", "package a\n\nconst A = 2\n")
	git("add", "a.go")
	git("update-index", "--cacheinfo", "160000,2222222222222222222222222222222222222222,vendor/lib")
	git("commit", "-q", "-m", "fix A and bump the submodule")
	item, err := CaseFromCommit(context.Background(), dir, git("rev-parse", "HEAD"), "")
	if err != nil || len(item.Files) != 1 || item.Files[0].Path != "a.go" {
		t.Fatalf("case = %+v, %v", item, err)
	}
}
