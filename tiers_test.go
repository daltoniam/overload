package overload

import (
	"strings"
	"testing"
)

func tieredWorkflow() ResolvedWorkflow {
	agent := func(name string) ResolvedAgent { return ResolvedAgent{Name: name} }
	return ResolvedWorkflow{
		Kind:      "pr_review",
		Agents:    []ResolvedAgent{agent("lead"), agent("security"), agent("tests")},
		SkipPaths: []string{"*.lock"},
		ReviewPolicy: ReviewPolicy{Tiers: &Tiers{
			SensitivePaths: []string{"**/migrations/**", "**/auth/**", "**/*.tf", ".github/workflows/**"},
			Trivial:        &Tier{MaxLines: 10, MaxFiles: 3, Action: TierSkip},
			Lite:           &Tier{MaxLines: 100, MaxFiles: 20, Agents: []string{"main"}},
		}},
	}
}

func TestChooseTier(t *testing.T) {
	workflow := tieredWorkflow()
	files := func(count, lines int) []FileChange {
		changes := make([]FileChange, count)
		for index := range changes {
			changes[index] = FileChange{Path: "app/file" + string(rune('a'+index)) + ".py", Lines: lines}
		}
		return changes
	}
	for _, test := range []struct {
		name    string
		changes []FileChange
		tier    string
		skip    bool
		agents  string
	}{
		{"trivial at the line limit", files(2, 5), TierTrivial, true, ""},
		{"one line over trivial", []FileChange{{Path: "a.py", Lines: 11}}, TierLite, false, "lead"},
		{"trivial at the file limit", files(3, 1), TierTrivial, true, ""},
		{"one file over trivial", files(4, 1), TierLite, false, "lead"},
		{"lite at its limits", files(20, 5), TierLite, false, "lead"},
		{"one line over lite", []FileChange{{Path: "a.py", Lines: 101}}, TierFull, false, ""},
		{"one file over lite", files(21, 1), TierFull, false, ""},
		{"skipped files do not count", append(files(1, 2), FileChange{Path: "poetry.lock", Lines: 900}), TierTrivial, true, ""},
		{"sensitive path forces full", []FileChange{{Path: "db/migrations/0042_add.py", Lines: 1}}, TierFull, false, ""},
		{"sensitive workflow file", []FileChange{{Path: ".github/workflows/ci.yml", Lines: 2}}, TierFull, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := workflow.ChooseTier(test.changes)
			if decision.Tier != test.tier || decision.Skip != test.skip || strings.Join(decision.Agents, ",") != test.agents || decision.Reason == "" {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
	if decision := (ResolvedWorkflow{}).ChooseTier(files(1, 1)); decision.Tier != TierFull {
		t.Fatalf("no tiers: %+v", decision)
	}
	narrowed := workflow.WithAgents([]string{"lead"})
	if len(narrowed.Agents) != 1 || narrowed.Agents[0].Name != "lead" || len(workflow.Agents) != 3 {
		t.Fatalf("narrowed = %+v", narrowed.Agents)
	}
}

func TestTierValidation(t *testing.T) {
	agents := []string{"lead", "security"}
	for name, tiers := range map[string]*Tiers{
		"skip with agents":   {Trivial: &Tier{MaxLines: 10, MaxFiles: 3, Action: TierSkip, Agents: []string{"main"}}},
		"lite skip":          {Lite: &Tier{MaxLines: 10, MaxFiles: 3, Action: TierSkip}},
		"no agents":          {Lite: &Tier{MaxLines: 10, MaxFiles: 3}},
		"unknown agent":      {Lite: &Tier{MaxLines: 10, MaxFiles: 3, Agents: []string{"main", "docs"}}},
		"without main":       {Lite: &Tier{MaxLines: 10, MaxFiles: 3, Agents: []string{"security"}}},
		"no files":           {Lite: &Tier{MaxLines: 10, Agents: []string{"main"}}},
		"trivial over lite":  {Trivial: &Tier{MaxLines: 50, MaxFiles: 3, Action: TierSkip}, Lite: &Tier{MaxLines: 10, MaxFiles: 3, Agents: []string{"main"}}},
		"bad sensitive glob": {SensitivePaths: []string{"/abs"}},
	} {
		if err := tiers.validate(agents); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := tieredWorkflow().Tiers.validate([]string{"lead", "security", "tests"}); err != nil {
		t.Fatal(err)
	}
}

func TestDiffChanges(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@\n ctx\n-old\n+new\n+more\ndiff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-x\n-y\n"
	changes := DiffChanges(patch)
	if len(changes) != 2 || changes[0] != (FileChange{Path: "a.go", Lines: 3}) || changes[1] != (FileChange{Path: "gone.go", Lines: 2}) {
		t.Fatalf("changes = %+v", changes)
	}
}
