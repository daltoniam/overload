package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/daltoniam/overload"
)

func TestLocalReviewPersistence(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	entry := "Review."
	workflow := overload.ResolvedWorkflow{Name: "context-v1", Kind: "pr_review", Agents: []overload.ResolvedAgent{{Name: "reviewer", Model: overload.ModelProfile{Provider: "openaicompat", Model: "test-model", BaseURL: "http://localhost:1234/v1"}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
	id, err := store.StartLocalReview(ctx, "test/local-review", 12, workflow)
	if err != nil {
		t.Fatal(err)
	}
	if routing, err := store.ReadRunRouting(ctx, id); err != nil || len(routing.Agents) != 0 {
		t.Fatalf("unfinished run routing=%+v error=%v", routing, err)
	}
	spec := overload.ReviewSpec{Repository: overload.Repository{FullName: "test/local-review"}, PRNumber: 12, Diff: "patch", Workflow: workflow}
	result := overload.ReviewResult{Summary: "One bug", Findings: []overload.Finding{{Path: "a.go", Line: 1, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Bug", Body: "Fix", Confidence: .9, Evidence: "bad()", Agents: []string{"reviewer", "security"}}}}
	result.Metrics = map[string]any{"routing": overload.Routing{Skipped: []string{"go.lock"}, Agents: []overload.AgentFiles{{Agent: "reviewer", Files: []string{"a.go"}, Reviewed: 1, Findings: 1, InputTokens: 10}}}}
	if err := store.FinishLocalReview(ctx, id, spec, result, "head-sha", nil); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, id)
	if err != nil || run.Status != overload.RunCompleted || run.HeadSHA != "head-sha" {
		t.Fatalf("run=%+v error=%v", run, err)
	}
	findings, err := store.ListFindings(ctx, id)
	if err != nil || len(findings) != 1 || findings[0].Title != "Bug" || len(findings[0].Agents) != 2 || findings[0].Agents[1] != "security" {
		t.Fatalf("findings=%+v error=%v", findings, err)
	}
	routing, err := store.ReadRunRouting(ctx, id)
	if err != nil || len(routing.Skipped) != 1 || routing.Agents[0].Files[0] != "a.go" || routing.Agents[0].InputTokens != 10 {
		t.Fatalf("routing=%+v error=%v", routing, err)
	}
	data, err := store.ReadArtifact(ctx, id, "result.json")
	if err != nil || len(data) == 0 {
		t.Fatalf("result artifact=%q error=%v", data, err)
	}
	if err := store.FinishLocalReview(ctx, id, spec, result, "head-sha", nil); err == nil {
		t.Fatal("duplicate finish accepted")
	}
}
