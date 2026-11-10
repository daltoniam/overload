package queue

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// fixedHeads answers every pull request of any repository with the same
// head and base, targeting host repository hostID.
type fixedHeads struct{ hostID int64 }

func (heads fixedHeads) PullRequest(_ context.Context, _ overload.Repository, number int) (overload.PullRequest, error) {
	return overload.PullRequest{Number: number, State: "open", HeadSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("a", 40), RepositoryID: heads.hostID}, nil
}

func headArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	zip := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zip)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: "repo/" + name, Mode: 0600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestRunEvalReportsCostAndRecall(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("eval-%d", time.Now().UnixNano())
	repo := "test/" + name
	agents := []string{name + "-finder", name + "-quiet"}
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, statement := range []string{
			`DELETE FROM findings WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`,
			`DELETE FROM run_artifacts WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`,
			`DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`,
			`DELETE FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`,
			`DELETE FROM repositories WHERE full_name=$1`,
		} {
			_, _ = store.Pool.Exec(cleanup, statement, repo)
		}
		for _, agent := range agents {
			_, _ = store.Pool.Exec(cleanup, `DELETE FROM workflows WHERE name=$1`, agent)
			_, _ = store.Pool.Exec(cleanup, `DELETE FROM agent_definitions WHERE name=$1`, agent)
			_, _ = store.Pool.Exec(cleanup, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+agent)
			_, _ = store.Pool.Exec(cleanup, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+agent)
		}
		_, _ = store.Pool.Exec(cleanup, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		result := `{"summary":"Nothing","findings":[]}`
		if strings.Contains(string(body), "Find null bugs") {
			result = `{"summary":"Null","findings":[{"path":"file.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Null pointer","body":"user may be nil","confidence":0.9,"evidence":"dangerous()"}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100}}`, result)
	}))
	defer model.Close()
	input, output := 2.0, 10.0
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: model.URL, Model: "m", PromptProfile: "context", PriceInput: &input, PriceOutput: &output}); err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		prompt := "Review quietly."
		if strings.HasSuffix(agent, "finder") {
			prompt = "Find null bugs."
		}
		if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: agent, Model: name, Prompt: prompt, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveWorkflow(ctx, overload.Workflow{Name: agent, Kind: "pr_review", Agents: []string{agent}, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveRepository(ctx, "", repo, false, true); err != nil {
		t.Fatal(err)
	}
	var known overload.Repository
	repos, err := store.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range repos {
		if candidate.Ref() == repo {
			known = candidate
		}
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	worker := &ReviewWorker{Store: store, Source: webhookSource{headArchive(t, map[string]string{"file.go": "user.Name()\n"})}}
	batch := name
	go func() {
		for ctx.Err() == nil {
			rows, err := store.Pool.Query(ctx, `SELECT id FROM runs WHERE trigger='eval' AND status='queued' AND repository_id=(SELECT id FROM repositories WHERE full_name=$1)`, repo)
			if err == nil {
				var ids []int64
				for rows.Next() {
					var id int64
					if rows.Scan(&id) == nil {
						ids = append(ids, id)
					}
				}
				rows.Close()
				for _, id := range ids {
					_ = worker.Work(ctx, &river.Job[postgres.ReviewArgs]{Args: postgres.ReviewArgs{RunID: id}})
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	results, err := RunEval(ctx, store, client, fixedHeads{}, EvalPlan{Repository: known, PRs: []int{7, 8}, Workflows: agents, Concurrency: 3, Batch: batch, Poll: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	report := eval.BuildReport(repo, []int{7, 8}, agents, results, []eval.Label{{PR: 7, Path: "file.go", Line: 2, Window: 1, Keywords: []string{"nil"}}})
	finder, quiet := report.Summary[0], report.Summary[1]
	// Each review is one call: 1,000 input x $2 + 100 output x $10 per million.
	if finder.Runs != 2 || finder.Completed != 2 || finder.Caught != 1 || finder.Recall != 1 || finder.FindingsPerPR != 1 || math.Abs(finder.CostTotalUSD-0.006) > 1e-9 || math.Abs(finder.CostP90USD-0.003) > 1e-9 {
		t.Fatalf("finder = %+v", finder)
	}
	if quiet.Completed != 2 || quiet.Caught != 0 || quiet.FindingsPerPR != 0 {
		t.Fatalf("quiet = %+v", quiet)
	}
	for _, run := range report.Runs {
		var dryRun bool
		var trigger string
		if err := store.Pool.QueryRow(ctx, `SELECT dry_run, trigger FROM runs WHERE id=$1`, run.RunID).Scan(&dryRun, &trigger); err != nil || !dryRun || trigger != postgres.EvalTrigger {
			t.Fatalf("run %d dry_run=%v trigger=%q err=%v", run.RunID, dryRun, trigger, err)
		}
	}
}

func TestRunEvalRefusesPullRequestsOfAnotherRepository(t *testing.T) {
	// The pull requests target host repository 7, not the 42 overload
	// knows (on GitHub or on a GitLab connection alike).
	for _, repo := range []overload.Repository{{FullName: "acme/api"}, {FullName: "group/api", ForgeID: 3, Forge: "work", ForgeKind: overload.ForgeGitLab}} {
		_, err := RunEval(context.Background(), nil, nil, fixedHeads{hostID: 7}, EvalPlan{Repository: repo, HostRepoID: 42, PRs: []int{7}, Workflows: []string{"sol"}})
		if err == nil || !strings.Contains(err.Error(), "not a pull request of the repository overload knows as "+repo.Ref()) {
			t.Fatalf("%s: err = %v", repo.Ref(), err)
		}
	}
}
