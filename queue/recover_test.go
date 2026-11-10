package queue

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/postgres/pgtest"
	reviewpkg "github.com/daltoniam/overload/review"
	gh "github.com/google/go-github/v75/github"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// stoppableSource blocks fetching the pull request until the job's context
// ends, like a review cut off part way by a server stopping.
type stoppableSource struct {
	webhookSource
	started chan<- struct{}
	calls   *atomic.Int32
}

func (source stoppableSource) PullRequestWithDiff(ctx context.Context, repository string, number int) (*gh.PullRequest, string, error) {
	source.calls.Add(1)
	source.started <- struct{}{}
	<-ctx.Done()
	return nil, "", ctx.Err()
}

type countingSource struct {
	webhookSource
	calls *atomic.Int32
}

func (source countingSource) PullRequestWithDiff(ctx context.Context, repository string, number int) (*gh.PullRequest, string, error) {
	source.calls.Add(1)
	return source.webhookSource.PullRequestWithDiff(ctx, repository, number)
}

// TestRecoveredRunReviewedOnce stops a server part way through a review and
// starts another: the review must run exactly once more, even when the old
// job was left retryable (as a hard stop or an older release leaves it).
func TestRecoveredRunReviewedOnce(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	var modelCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		result := `{"summary":"Looks fine","findings":[]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, result)
	}))
	defer model.Close()
	name, repo := "recover-once", "test/recover-once"
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: model.URL, Model: "test", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Model: name, Prompt: "Review the change", Enabled: true}); err != nil {
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
	source := webhookSource{archive: reviewArchive(t)}

	started := make(chan struct{}, 1)
	var firstCalls, secondCalls atomic.Int32
	first := reviewClient(t, store, stoppableSource{webhookSource: source, started: started, calls: &firstCalls})
	sha := strings.Repeat("b", 40)
	if accepted, err := store.IngestPR(ctx, first, postgres.PullRequestDelivery{DeliveryID: name, Action: "opened", Payload: []byte(`{}`), RepoName: repo, PR: 7, HeadSHA: sha, BaseSHA: strings.Repeat("a", 40), Eligible: true}); err != nil || !accepted {
		t.Fatalf("ingest: %v %v", accepted, err)
	}
	var runID, oldJobID int64
	if err := store.Pool.QueryRow(ctx, `SELECT id, river_job_id FROM runs WHERE head_sha = $1`, sha).Scan(&runID, &oldJobID); err != nil {
		t.Fatal(err)
	}
	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	if err := first.Start(firstCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the first server never started the review")
	}
	stopFirst()
	select {
	case <-first.Stopped():
	case <-time.After(30 * time.Second):
		t.Fatal("the first server did not stop")
	}
	if status := runStatus(t, store, runID); status != "running" {
		t.Fatalf("run after the stop is %q, want running so the next start queues it again", status)
	}
	if state := jobState(t, store, oldJobID); state != "cancelled" {
		t.Fatalf("interrupted job is %q, want cancelled", state)
	}
	// Leave the old job due for a retry, as a hard stop or an older release
	// would, to check that recovery cancels it.
	if _, err := store.Pool.Exec(ctx, `UPDATE river_job SET state = 'retryable', finalized_at = NULL, scheduled_at = now() WHERE id = $1`, oldJobID); err != nil {
		t.Fatal(err)
	}

	second := reviewClient(t, store, countingSource{webhookSource: source, calls: &secondCalls})
	if recovered, err := store.RecoverInterruptedRuns(ctx, second); err != nil || recovered != 1 {
		t.Fatalf("recover: %d %v", recovered, err)
	}
	secondCtx, stopSecond := context.WithCancel(ctx)
	if err := second.Start(secondCtx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopSecond()
		<-second.Stopped()
	}()
	deadline := time.Now().Add(30 * time.Second)
	for runStatus(t, store, runID) != "completed" {
		if time.Now().After(deadline) {
			t.Fatalf("recovered run is %q, want completed", runStatus(t, store, runID))
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Give a duplicate job time to be fetched and worked if one were left.
	time.Sleep(2 * time.Second)
	if status := runStatus(t, store, runID); status != "completed" {
		t.Fatalf("run is %q after the recovered review finished, want completed", status)
	}
	if calls := secondCalls.Load(); calls != 1 {
		t.Fatalf("the restarted server reviewed the run %d times, want 1", calls)
	}
	if calls := firstCalls.Load(); calls != 1 {
		t.Fatalf("the stopped server reviewed the run %d times, want 1", calls)
	}
	if calls := modelCalls.Load(); calls < 1 {
		t.Fatal("the recovered review never called the model")
	}
	if state := jobState(t, store, oldJobID); state != "cancelled" {
		t.Fatalf("interrupted job is %q after recovery, want cancelled", state)
	}
	var completed, other int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'completed'), count(*) FILTER (WHERE state NOT IN ('completed', 'cancelled')) FROM river_job WHERE kind = 'review_pr' AND (args->>'run_id')::bigint = $1`, runID).Scan(&completed, &other); err != nil {
		t.Fatal(err)
	}
	if completed != 1 || other != 0 {
		t.Fatalf("jobs for the run: %d completed and %d still pending, want 1 and 0", completed, other)
	}
}

func reviewClient(t *testing.T, store *postgres.Store, source reviewpkg.PullRequestSource) *river.Client[pgx.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, &ReviewWorker{Store: store, Source: source})
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers:           workers,
		FetchPollInterval: 50 * time.Millisecond,
		FetchCooldown:     10 * time.Millisecond,
		SoftStopTimeout:   100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func reviewArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	zip := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zip)
	file := []byte("dangerous()\n")
	if err := writer.WriteHeader(&tar.Header{Name: "repo-prefix/file.go", Mode: 0o600, Size: int64(len(file))}); err != nil {
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
	return archive.Bytes()
}

func runStatus(t *testing.T, store *postgres.Store, runID int64) string {
	t.Helper()
	var status string
	if err := store.Pool.QueryRow(context.Background(), `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func jobState(t *testing.T, store *postgres.Store, jobID int64) string {
	t.Helper()
	var state string
	if err := store.Pool.QueryRow(context.Background(), `SELECT state FROM river_job WHERE id = $1`, jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
