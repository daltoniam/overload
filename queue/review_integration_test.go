package queue

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	gh "github.com/google/go-github/v75/github"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

type webhookSource struct{ archive []byte }

func (source webhookSource) PullRequestWithDiff(_ context.Context, _ string, number int) (*gh.PullRequest, string, error) {
	pr := &gh.PullRequest{Number: gh.Ptr(number), State: gh.Ptr("open"), Head: &gh.PullRequestBranch{SHA: gh.Ptr(strings.Repeat("b", 40))}, Base: &gh.PullRequestBranch{SHA: gh.Ptr(strings.Repeat("a", 40))}}
	return pr, "diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old()\n+dangerous()\n", nil
}

func (source webhookSource) DownloadHead(_ context.Context, _, _ string, writer io.Writer) error {
	_, err := writer.Write(source.archive)
	return err
}

func TestBoundWebhookWorkerDryRun(t *testing.T) {
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
	name := fmt.Sprintf("webhook-worker-%d", time.Now().UnixNano())
	repo := "test/" + name
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `UPDATE runs SET binding_id=NULL WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM findings WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM run_artifacts WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`, repo)
		_, _ = store.Pool.Exec(ctx, `UPDATE webhook_deliveries SET run_id=NULL WHERE repository_full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE repository_full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM trigger_bindings WHERE repository_full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		content := fmt.Sprint(request.Messages)
		if !strings.Contains(content, "Pinned PR entry") || !strings.Contains(content, "Pinned PR review") || strings.Contains(content, "Edited PR entry") {
			t.Errorf("unexpected prompt: %s", content)
		}
		result := `{"summary":"Issue","findings":[{"path":"file.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bug","body":"Fix it","confidence":0.9,"evidence":"dangerous()"}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, result)
	}))
	defer model.Close()
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: model.URL, Model: "test", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []overload.PromptTemplate{{Name: name, Kind: "entry", Body: "Pinned PR entry"}, {Name: name, Kind: "review", Body: "Pinned PR review"}} {
		if _, err := store.SavePrompt(ctx, prompt); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Model: name, EntryPrompt: name, ReviewPrompt: name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "pr_review", Agents: []string{name}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRepository(ctx, repo, true, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBinding(ctx, overload.TriggerBinding{Source: "github", Event: "pull_request", Action: "opened", Repository: repo, Workflow: name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	if accepted, err := store.IngestPR(ctx, client, postgres.PullRequestDelivery{DeliveryID: name, Action: "opened", Payload: []byte(`{}`), RepoName: repo, PR: 42, HeadSHA: sha, BaseSHA: strings.Repeat("a", 40), Eligible: true}); err != nil || !accepted {
		t.Fatalf("ingest: %v %v", accepted, err)
	}
	var runID int64
	if err := store.Pool.QueryRow(ctx, `SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1) AND head_sha=$2 ORDER BY id DESC LIMIT 1`, repo, sha).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePrompt(ctx, overload.PromptTemplate{Name: name, Kind: "entry", Body: "Edited PR entry"}); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	zip := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zip)
	file := []byte("dangerous()\n")
	if err := writer.WriteHeader(&tar.Header{Name: "repo-prefix/file.go", Mode: 0600, Size: int64(len(file))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	worker := &ReviewWorker{Store: store, Source: webhookSource{archive.Bytes()}}
	job := &river.Job[postgres.ReviewArgs]{Args: postgres.ReviewArgs{RunID: runID}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil || run.Status != "completed" || !run.DryRun || calls != 1 {
		t.Fatalf("run=%+v calls=%d err=%v", run, calls, err)
	}
	findings, err := store.ListFindings(ctx, runID)
	if err != nil || len(findings) != 1 || findings[0].Path != "file.go" || strings.Join(findings[0].Agents, ",") != name {
		t.Fatalf("findings=%+v err=%v", findings, err)
	}
	for _, artifact := range []string{"spec.json", "result.json"} {
		data, err := store.ReadArtifact(ctx, runID, artifact)
		if err != nil || !json.Valid(data) {
			t.Fatalf("%s: %s %v", artifact, data, err)
		}
	}
	if err := worker.Work(ctx, job); err != nil || calls != 1 {
		t.Fatalf("duplicate worker: %d %v", calls, err)
	}
	if accepted, err := store.IngestPR(ctx, client, postgres.PullRequestDelivery{DeliveryID: name, Action: "opened", Payload: []byte(`{}`), RepoName: repo, PR: 42, HeadSHA: sha, BaseSHA: strings.Repeat("a", 40), Eligible: true}); err != nil || accepted {
		t.Fatalf("duplicate delivery: %v %v", accepted, err)
	}
}
