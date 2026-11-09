package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	if score.Expected != 5 || score.Matched != 2 || score.Misplaced != 1 || score.Unmatched != 2 {
		t.Fatalf("score = %+v", score)
	}
	if _, err := invoice.Repository().Open("billing/invoice.go"); err != nil {
		t.Fatal(err)
	}
}

func TestSuiteValidate(t *testing.T) {
	good := PlantedSuite().Cases[1]
	for name, suite := range map[string]Suite{
		"no cases":      {Name: "s"},
		"bad name":      {Name: "a b", Cases: []Case{good}},
		"repeated case": {Name: "s", Cases: []Case{good, good}},
		"unknown file":  {Name: "s", Cases: []Case{{Name: "c", Files: good.Files, Diff: good.Diff, Expected: []ExpectedFinding{{Path: "nope.go", StartLine: 1, EndLine: 1}}}}},
		"backwards":     {Name: "s", Cases: []Case{{Name: "c", Files: good.Files, Diff: good.Diff, Expected: []ExpectedFinding{{Path: "util/textutil.go", StartLine: 5, EndLine: 4}}}}},
		"escaping path": {Name: "s", Cases: []Case{{Name: "c", Files: []CaseFile{{Path: "../x.go"}}, Diff: good.Diff, Expected: good.Expected}}},
		"too large":     {Name: "s", Cases: []Case{{Name: "c", Files: []CaseFile{{Path: "util/textutil.go", Content: strings.Repeat("x", MaxCaseBytes)}}, Diff: good.Diff, Expected: good.Expected}}},
		"missing diff":  {Name: "s", Cases: []Case{{Name: "c", Files: good.Files, Expected: good.Expected}}},
	} {
		if err := suite.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
}

func TestCaseFromCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
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
	git("init", "-q")
	write("calc/sum.go", "package calc\n\n// Sum adds the values.\nfunc Sum(values []int) int {\n\ttotal := 0\n\tfor i := 1; i < len(values); i++ {\n\t\ttotal += values[i]\n\t}\n\treturn total\n}\n")
	write("README.md", "calc\n")
	git("add", ".")
	git("commit", "-q", "-m", "add sum")
	write("calc/sum.go", "package calc\n\n// Sum adds the values.\nfunc Sum(values []int) int {\n\ttotal := 0\n\tfor i := 0; i < len(values); i++ {\n\t\ttotal += values[i]\n\t}\n\treturn total\n}\n")
	write("NEW.md", "added\n")
	git("add", ".")
	git("commit", "-q", "-m", "Fix Sum skipping the first value")
	fix := git("rev-parse", "HEAD")

	item, err := CaseFromCommit(context.Background(), dir, fix[:10], "")
	if err != nil {
		t.Fatal(err)
	}
	if item.Name != "commit-"+fix[:12] || item.Description != "Fix Sum skipping the first value" || len(item.Files) != 1 || item.Files[0].Path != "calc/sum.go" {
		t.Fatalf("case = %+v", item)
	}
	if !strings.Contains(item.Files[0].Content, "i := 1") || !strings.Contains(item.Diff, "+\tfor i := 1") || !strings.Contains(item.Diff, "-\tfor i := 0") {
		t.Fatalf("the case does not show the buggy version:\n%s", item.Diff)
	}
	if len(item.Expected) != 1 || item.Expected[0].StartLine != 6 || item.Expected[0].EndLine != 6 {
		t.Fatalf("expected = %+v", item.Expected)
	}
	if score := ScoreFindings(item.Expected, []overload.Finding{{Path: "calc/sum.go", Line: 6}}, 0); score.Matched != 1 {
		t.Fatalf("score = %+v", score)
	}
	for _, commit := range []string{"", "HEAD", "--upload-pack=x", "zzzzzzz"} {
		if _, err := CaseFromCommit(context.Background(), dir, commit, ""); err == nil {
			t.Errorf("%q accepted", commit)
		}
	}
	first := git("rev-list", "--max-parents=0", "HEAD")
	if _, err := CaseFromCommit(context.Background(), dir, first, ""); err == nil {
		t.Error("a root commit was accepted")
	}
}
