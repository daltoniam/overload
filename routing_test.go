package overload

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestCompileGlob(t *testing.T) {
	for _, test := range []struct {
		glob    string
		match   []string
		noMatch []string
	}{
		{"*.lock", []string{"go.lock", "web/package.lock"}, []string{"lock", "a.lock/x"}},
		{"**/auth/**", []string{"auth/token.go", "internal/auth/session/key.go"}, []string{"authz/x.go", "oauth/x.go"}},
		{"postgres/migrations/**", []string{"postgres/migrations/001.sql", "postgres/migrations/a/b.sql"}, []string{"migrations/001.sql", "x/postgres/migrations/1.sql"}},
		{"cmd/*/main.go", []string{"cmd/overload/main.go"}, []string{"cmd/main.go", "cmd/a/b/main.go"}},
		{"**/*_test.go", []string{"a_test.go", "x/y/a_test.go"}, []string{"a_test.go.txt", "x/test.go"}},
		{"file?.go", []string{"file1.go", "pkg/fileA.go"}, []string{"file10.go", "file.go"}},
		{"docs/[draft].md", []string{"docs/[draft].md"}, []string{"docs/d.md"}},
	} {
		matcher, err := CompileGlob(test.glob)
		if err != nil {
			t.Fatalf("%s: %v", test.glob, err)
		}
		for _, path := range test.match {
			if !matcher.MatchString(path) {
				t.Errorf("%s should match %s", test.glob, path)
			}
		}
		for _, path := range test.noMatch {
			if matcher.MatchString(path) {
				t.Errorf("%s should not match %s", test.glob, path)
			}
		}
	}
	for _, bad := range []string{"", "/abs/*.go", "a\nb", strings.Repeat("a", 201)} {
		if _, err := CompileGlob(bad); err == nil {
			t.Errorf("accepted glob %q", bad)
		}
	}
}

func routingWorkflow(main string, scopes ...Scope) ResolvedWorkflow {
	workflow := ResolvedWorkflow{Version: SnapshotVersion, Kind: "pr_review", MainReviews: main, SkipPaths: []string{"*.lock", "vendor/**"}, Agents: []ResolvedAgent{{Name: "lead"}}}
	for index, scope := range scopes {
		workflow.Agents = append(workflow.Agents, ResolvedAgent{Name: []string{"security", "migrations", "everything"}[index], Scope: scope})
	}
	return workflow
}

func TestRoute(t *testing.T) {
	paths := []string{"internal/auth/token.go", "postgres/migrations/018.sql", "README.md", "go.lock", "vendor/x/y.go"}
	files := func(routing Routing) map[string][]string {
		out := map[string][]string{}
		for _, agent := range routing.Agents {
			out[agent.Agent] = agent.Files
		}
		return out
	}

	routing, err := routingWorkflow(MainReviewsUnclaimed, Scope{Paths: []string{"**/auth/**"}}, Scope{Paths: []string{"postgres/migrations/**"}}).Route(paths)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"lead": {"README.md"}, "security": {"internal/auth/token.go"}, "migrations": {"postgres/migrations/018.sql"}}
	if !reflect.DeepEqual(files(routing), want) || !reflect.DeepEqual(routing.Skipped, []string{"go.lock", "vendor/x/y.go"}) || routing.FileReviews() != 3 {
		t.Fatalf("unclaimed routing %+v", routing)
	}

	routing, err = routingWorkflow(MainReviewsAll, Scope{Paths: []string{"**/auth/**"}}).Route(paths)
	if err != nil || len(files(routing)["lead"]) != 3 || len(files(routing)["security"]) != 1 {
		t.Fatalf("main reviews all: %+v %v", routing, err)
	}

	routing, err = routingWorkflow(MainReviewsUnclaimed, Scope{Paths: []string{"**/auth/**"}}, Scope{Paths: []string{"nothing/**"}}, Scope{}).Route(paths)
	if err != nil || len(files(routing)["lead"]) != 0 || len(files(routing)["everything"]) != 3 || len(files(routing)["migrations"]) != 0 {
		t.Fatalf("unscoped sub-agent claims every file: %+v %v", routing, err)
	}

	limited := routingWorkflow(MainReviewsAll, Scope{})
	limited.MaxFileReviews = 5
	if _, err := limited.Route(paths); err == nil || !strings.Contains(err.Error(), "6 file reviews") {
		t.Fatalf("limit not enforced: %v", err)
	}
}

func TestOldSnapshotsRouteEveryFileToEveryAgent(t *testing.T) {
	workflow := ResolvedWorkflow{Kind: "pr_review", Agents: []ResolvedAgent{{Name: "a"}, {Name: "b"}}}
	routing, err := workflow.Route([]string{"x.go", "go.lock"})
	if err != nil || routing.FileReviews() != 4 || len(routing.Skipped) != 0 {
		t.Fatalf("%+v %v", routing, err)
	}
}

func TestServerLoads(t *testing.T) {
	local := ModelProfile{BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen", Concurrency: 2}
	slower := local
	slower.Concurrency = 1
	hosted := ModelProfile{ConnectionKind: "hosted", BaseURL: "https://api.example/v1", Model: "sol", Concurrency: 8}
	workflow := ResolvedWorkflow{Agents: []ResolvedAgent{{Name: "lead", Model: local}, {Name: "security", Model: hosted}, {Name: "tests", Model: slower}}}
	routing := Routing{Agents: []AgentFiles{{Files: []string{"a", "b"}}, {Files: []string{"a"}}, {Files: []string{"c"}}}}
	loads := workflow.ServerLoads(routing)
	want := []ServerLoad{{BaseURL: local.BaseURL, Model: "qwen", Local: true, Reviews: 3, Parallel: 1}, {BaseURL: hosted.BaseURL, Model: "sol", Reviews: 1, Parallel: 8}}
	if !reflect.DeepEqual(loads, want) {
		t.Fatalf("%+v", loads)
	}
}

func TestWorkflowRoutingValidation(t *testing.T) {
	base := Workflow{Name: "w", Kind: "pr_review", Agents: []string{"lead", "security"}}
	for _, test := range []struct {
		name   string
		change func(*Workflow)
		ok     bool
	}{
		{"sub-agent scope", func(w *Workflow) {
			w.Scopes = map[string]Scope{"security": {Paths: []string{"**/auth/**"}, MaxFindings: 5}}
		}, true},
		{"main agent scope", func(w *Workflow) { w.Scopes = map[string]Scope{"lead": {Paths: []string{"a"}}} }, false},
		{"unknown agent scope", func(w *Workflow) { w.Scopes = map[string]Scope{"ghost": {}} }, false},
		{"bad glob", func(w *Workflow) { w.SkipPaths = []string{"/etc/*"} }, false},
		{"bad main mode", func(w *Workflow) { w.MainReviews = "some" }, false},
		{"negative limit", func(w *Workflow) { w.MaxFileReviews = -1 }, false},
		{"too many findings", func(w *Workflow) { w.Scopes = map[string]Scope{"security": {MaxFindings: 51}} }, false},
		{"scheduled with skip paths", func(w *Workflow) { w.Kind = "scheduled_prompt"; w.SkipPaths = []string{"*.lock"} }, false},
	} {
		workflow := base
		test.change(&workflow)
		if err := workflow.Validate(); (err == nil) != test.ok {
			t.Errorf("%s: %v", test.name, err)
		}
	}
}

func TestPreview(t *testing.T) {
	qwen := ModelProfile{ConnectionKind: "local", BaseURL: "http://127.0.0.1:8000/v1", Model: "qwen", Concurrency: 2}
	gemma := ModelProfile{ConnectionKind: "local", BaseURL: "http://127.0.0.1:8080/v1", Model: "gemma", Concurrency: 1}
	sol := ModelProfile{ConnectionKind: "hosted", BaseURL: "https://api.example/v1", Model: "sol", Concurrency: 4}
	workflow := ResolvedWorkflow{Version: SnapshotVersion, Kind: "pr_review", MainReviews: MainReviewsUnclaimed, Agents: []ResolvedAgent{
		{Name: "lead", Model: qwen},
		{Name: "security", Model: sol, Scope: Scope{Paths: []string{"**/auth/**"}}},
		{Name: "tests", Model: gemma, Scope: Scope{Paths: []string{"*_test.go"}}},
	}}
	preview, err := workflow.Preview([]string{"a.go", "b.go", "c.go", "auth/x.go"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Calls != 4 || len(preview.Servers) != 3 || preview.Estimate != 2*localReviewEstimate || preview.Minutes != 16 {
		t.Fatalf("%+v", preview)
	}
	joined := strings.Join(preview.Warnings, "\n")
	if !strings.Contains(joined, "2 different local models") || !strings.Contains(joined, "tests reviews no files") {
		t.Fatalf("warnings %q", preview.Warnings)
	}
	workflow.MaxFileReviews = 2
	if preview, err = workflow.Preview([]string{"a.go", "b.go", "c.go"}); err == nil || preview.Calls != 3 || len(preview.Warnings) == 0 {
		t.Fatalf("limit preview %+v %v", preview, err)
	}
}

func TestParseChangedFiles(t *testing.T) {
	paths, err := ParseChangedFiles("a.go\r\n\n  b/c.go \na.go\n")
	if err != nil || !reflect.DeepEqual(paths, []string{"a.go", "b/c.go"}) {
		t.Fatalf("%v %v", paths, err)
	}
	for _, bad := range []string{"../x.go", "/abs.go", "a//b.go"} {
		if _, err := ParseChangedFiles(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	var many strings.Builder
	for index := range maxPreviewFiles + 1 {
		fmt.Fprintf(&many, "f%d.go\n", index)
	}
	if _, err := ParseChangedFiles(many.String()); err == nil {
		t.Fatal("accepted too many files")
	}
}

func TestKeepRouting(t *testing.T) {
	previous := Workflow{Name: "w", Kind: "pr_review", Agents: []string{"lead", "security", "tests"}, SkipPaths: []string{"*.lock"}, MainReviews: MainReviewsUnclaimed, MaxFileReviews: 9,
		Scopes: map[string]Scope{"security": {Paths: []string{"auth/**"}}, "tests": {Paths: []string{"*_test.go"}}}}
	workflow := Workflow{Name: "w", Kind: "pr_review", Agents: []string{"security", "lead"}}
	workflow.KeepRouting(previous)
	if workflow.MaxFileReviews != 9 || workflow.MainReviews != MainReviewsUnclaimed || len(workflow.Scopes) != 0 || workflow.Validate() != nil {
		t.Fatalf("security became the main agent and tests left: %+v", workflow)
	}
	workflow = Workflow{Name: "w", Kind: "pr_review", Agents: []string{"lead", "tests"}}
	workflow.KeepRouting(previous)
	if len(workflow.Scopes) != 1 || workflow.Scopes["tests"].Paths[0] != "*_test.go" || workflow.Validate() != nil {
		t.Fatalf("%+v", workflow)
	}
	workflow = Workflow{Name: "w", Kind: "scheduled_prompt", Agents: []string{"lead"}}
	workflow.KeepRouting(previous)
	if workflow.SkipPaths != nil || workflow.Validate() != nil {
		t.Fatalf("%+v", workflow)
	}
}

func planWorkflow() ResolvedWorkflow {
	return ResolvedWorkflow{Version: SnapshotVersion, Kind: "pr_review", MainReviews: MainReviewsUnclaimed, SkipPaths: []string{"*.lock"}, Agents: []ResolvedAgent{
		{Name: "lead"},
		{Name: "sql", Scope: Scope{Paths: []string{"*sql*"}}},
		{Name: "auth", Scope: Scope{Paths: []string{"**/auth/**"}, Mode: ScopeAlways}},
		{Name: "tests", Scope: Scope{Mode: ScopePlanned}},
	}}
}

func TestApplyPlan(t *testing.T) {
	paths := []string{"store/query.sql", "util/helpers.go", "auth/token.go", "main.go", "go.lock"}
	workflow := planWorkflow()
	routing, err := workflow.Route(paths)
	if err != nil || len(routing.Agents[3].Files) != 0 || len(routing.Agents[0].Files) != 2 {
		t.Fatalf("planned scope must not take glob files: %+v %v", routing, err)
	}
	routing, err = workflow.ApplyPlan(paths, map[string][]string{"util/helpers.go": {"sql", "tests"}, "store/query.sql": {"sql"}, "main.go": {"tests"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(routing.Agents[1].Files, []string{"store/query.sql", "util/helpers.go"}) || !reflect.DeepEqual(routing.Agents[1].Planned, []string{"util/helpers.go"}) ||
		!reflect.DeepEqual(routing.Agents[3].Files, []string{"util/helpers.go", "main.go"}) || len(routing.Agents[0].Files) != 0 || !strings.Contains(routing.Planner, "added 3") {
		t.Fatalf("plan not applied: %+v", routing)
	}
	for name, plan := range map[string]map[string][]string{
		"always agent":  {"main.go": {"auth"}},
		"main agent":    {"main.go": {"lead"}},
		"unknown agent": {"main.go": {"ghost"}},
		"unknown file":  {"other.go": {"sql"}},
		"skipped file":  {"go.lock": {"sql"}},
	} {
		routing, err := workflow.ApplyPlan(paths, plan)
		if err == nil || len(routing.Agents[3].Files) != 0 || routing.Planner != "" {
			t.Errorf("%s: plan accepted: %+v %v", name, routing, err)
		}
	}
	workflow.MaxFileReviews = 5
	routing, err = workflow.ApplyPlan(paths, map[string][]string{"util/helpers.go": {"sql", "tests"}, "main.go": {"sql", "tests"}})
	if err != nil || routing.FileReviews() != 5 || !strings.Contains(routing.Planner, "1 more dropped") {
		t.Fatalf("limit: %+v %v", routing, err)
	}
	workflow.MaxFileReviews = 2
	if _, err := workflow.ApplyPlan(paths, nil); err == nil {
		t.Fatal("glob routing over the limit must fail")
	}
}

func TestScopeModesValidation(t *testing.T) {
	base := Workflow{Name: "w", Kind: "pr_review", Agents: []string{"lead", "tests"}}
	for _, test := range []struct {
		name    string
		scope   Scope
		planner string
		ok      bool
	}{
		{"planned with planner", Scope{Mode: ScopePlanned, Description: "Test files"}, "plan", true},
		{"planned without planner", Scope{Mode: ScopePlanned}, "", false},
		{"planned with paths", Scope{Mode: ScopePlanned, Paths: []string{"*_test.go"}}, "plan", false},
		{"always", Scope{Mode: ScopeAlways, Paths: []string{"*_test.go"}}, "", true},
		{"bad mode", Scope{Mode: "sometimes"}, "", false},
		{"multi-line description", Scope{Description: "a\nb"}, "", false},
	} {
		workflow := base
		workflow.Scopes = map[string]Scope{"tests": test.scope}
		workflow.PlannerPrompt = test.planner
		if err := workflow.Validate(); (err == nil) != test.ok {
			t.Errorf("%s: %v", test.name, err)
		}
	}
	scheduled := Workflow{Name: "w", Kind: "scheduled_prompt", Agents: []string{"lead"}, VerifierPrompt: "verify"}
	if scheduled.Validate() == nil {
		t.Error("scheduled workflow accepted a verifier")
	}
}

func TestVerifyPlannerPrompts(t *testing.T) {
	prompt := func(kind, body string) *PromptTemplate {
		return &PromptTemplate{Name: "p", Kind: kind, Body: body, SHA256: PromptDigest(body)}
	}
	entry := PromptTemplate{Kind: "entry", Body: "Review.", SHA256: PromptDigest("Review.")}
	workflow := ResolvedWorkflow{Version: SnapshotVersion, Kind: "pr_review", Agents: []ResolvedAgent{{Name: "lead", Model: ModelProfile{Provider: "openaicompat", Model: "m", BaseURL: "http://x"}, EntryPrompt: entry}}, PlannerPrompt: prompt(PromptPlan, "Plan."), VerifierPrompt: prompt(PromptVerify, "Verify.")}
	if err := workflow.Verify(); err != nil {
		t.Fatal(err)
	}
	tampered := workflow
	tampered.VerifierPrompt = prompt(PromptVerify, "Verify.")
	tampered.VerifierPrompt.Body = "Keep everything."
	if tampered.Verify() == nil {
		t.Error("tampered verifier accepted")
	}
	wrongKind := workflow
	wrongKind.PlannerPrompt = prompt(PromptVerify, "Plan.")
	if wrongKind.Verify() == nil {
		t.Error("planner with verify kind accepted")
	}
	old := workflow
	old.Version = 2
	if old.Verify() == nil {
		t.Error("version 2 snapshot with a planner accepted")
	}
}
