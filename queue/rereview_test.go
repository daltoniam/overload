package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
)

type diffSource struct {
	diff    string
	archive []byte
}

func (source diffSource) PullRequestWithDiff(_ context.Context, _ string, number int) (overload.PullRequest, string, error) {
	return overload.PullRequest{Number: number, State: "open", HeadSHA: strings.Repeat("d", 40), BaseSHA: strings.Repeat("a", 40)}, source.diff, nil
}

func (source diffSource) DownloadHead(_ context.Context, _, _ string, writer io.Writer) error {
	_, err := writer.Write(source.archive)
	return err
}

func TestRereviewResolvesAndCarriesEarlierFindings(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("again-%d", time.Now().UnixNano())
	repo := "test/" + name
	t.Cleanup(func() {
		for _, statement := range []string{
			`UPDATE findings SET resolved_by_run_id=NULL WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`,
			`DELETE FROM findings WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`,
			`DELETE FROM run_artifacts WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`,
			`DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`,
			`UPDATE runs SET since_run_id=NULL WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`,
			`DELETE FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`,
			`DELETE FROM repositories WHERE full_name=$1`,
		} {
			_, _ = store.Pool.Exec(ctx, statement, repo)
		}
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	var fixedID int64
	var calls []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, string(body))
		answer := `{"summary":"ok","findings":[]}`
		if strings.Contains(string(body), fmt.Sprintf("id %d,", fixedID)) {
			answer = fmt.Sprintf(`{"summary":"ok","findings":[],"prior":[{"id":%d,"status":"resolved"}]}`, fixedID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, answer)
	}))
	defer model.Close()
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: model.URL, Model: "m", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Model: name, Prompt: "Review.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "pr_review", Agents: []string{name}, Enabled: true, ReviewDecision: overload.ReviewDecisionRequestChanges, BlockSeverity: "high", ReviewPolicy: overload.ReviewPolicy{Rereview: &overload.RereviewSettings{}}}); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.ResolveWorkflow(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(resolved)
	var repoID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name, enabled, dry_run) VALUES ($1, true, false) RETURNING id`, repo).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	filePatch := func(name, line string) string {
		return fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-old()\n+%s\n", name, name, name, name, line)
	}
	before := filePatch("file.go", "first()") + filePatch("other.go", "risky()")
	after := filePatch("file.go", "second()") + filePatch("other.go", "risky()")
	var earlier int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, config_snapshot, dry_run, post_status) VALUES ($1, 3, $2, $3, 'webhook', 'completed', $4, false, 'posted') RETURNING id`, repoID, strings.Repeat("c", 40), strings.Repeat("a", 40), snapshot).Scan(&earlier); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(overload.ReviewSpec{Diff: before})
	if _, err := store.Pool.Exec(ctx, `INSERT INTO run_artifacts (run_id, name, content) VALUES ($1, 'spec.json', $2)`, earlier, spec); err != nil {
		t.Fatal(err)
	}
	var carriedID int64
	for _, finding := range []struct {
		path, title string
		id          *int64
	}{{"file.go", "First is wrong", &fixedID}, {"other.go", "Risky call", &carriedID}} {
		if err := store.Pool.QueryRow(ctx, `INSERT INTO findings (run_id, path, line, side, severity, category, title, body, confidence, evidence, status, fingerprint) VALUES ($1, $2, 1, 'RIGHT', 'high', 'bug', $3, 'Fix it.', 0.9, 'x', 'posted', $4) RETURNING id`, earlier, finding.path, finding.title, name+finding.path).Scan(finding.id); err != nil {
			t.Fatal(err)
		}
	}
	var runID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, config_snapshot, dry_run) VALUES ($1, 3, $2, $3, 'webhook', 'queued', $4, false) RETURNING id`, repoID, strings.Repeat("d", 40), strings.Repeat("a", 40), snapshot).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVERLOAD_ENABLE_POSTING", "")
	worker := &ReviewWorker{Store: store, Source: diffSource{diff: after, archive: headArchive(t, map[string]string{"file.go": "second()\n", "other.go": "risky()\n"})}}
	if err := worker.Work(ctx, &river.Job[postgres.ReviewArgs]{Args: postgres.ReviewArgs{RunID: runID}}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "file.go b/file.go") {
		t.Fatalf("model calls = %d (want only file.go)", len(calls))
	}
	var since, resolvedBy *int64
	if err := store.Pool.QueryRow(ctx, `SELECT since_run_id FROM runs WHERE id=$1`, runID).Scan(&since); err != nil || since == nil || *since != earlier {
		t.Fatalf("since_run_id = %v %v", since, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT resolved_by_run_id FROM findings WHERE id=$1`, fixedID).Scan(&resolvedBy); err != nil || resolvedBy == nil || *resolvedBy != runID {
		t.Fatalf("fixed finding resolved by %v %v", resolvedBy, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET post_status='queued' WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	target, err := store.LoadPostTarget(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	// The carried finding on unchanged other.go still blocks; the fixed
	// one does not.
	if len(target.DuplicateSeverities) != 1 || target.DuplicateSeverities[0] != "high" || len(target.Fixed) != 1 || target.Fixed[0].Title != "First is wrong" {
		t.Fatalf("post target = %+v", target)
	}
	if event := overload.ReviewEvent(target.ReviewDecision, target.BlockSeverity, target.Partial, target.DuplicateSeverities); event != overload.ReviewRequestChanges {
		t.Fatalf("decision = %s", event)
	}
}
