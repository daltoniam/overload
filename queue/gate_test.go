package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

const (
	readableDiff   = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n"
	unreadableDiff = "diff --git a/a.go b/b.go\nold mode 100644\nnew mode 100755\n"
)

// sandboxClaim is a gate_files value a sandbox could write to pass the gate.
var sandboxClaim = overload.GateFiles{Changed: []string{"a.go"}, Commentable: []string{"a.go"}}

func TestRecordGateFilesIgnoresTheSandbox(t *testing.T) {
	for _, test := range []struct {
		name, diff string
		want       *overload.GateFiles
	}{
		{"readable diff", readableDiff, &overload.GateFiles{Changed: []string{"a.go"}, Commentable: []string{"a.go"}}},
		{"unreadable diff", unreadableDiff, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := overload.ReviewResult{Metrics: map[string]any{gateFilesMetric: overload.GateFiles{Changed: []string{"x.go"}, Commentable: []string{"x.go"}}}}
			files := recordGateFiles(test.diff, &result)
			stored, ok := result.Metrics[gateFilesMetric]
			if test.want == nil {
				if files != nil || ok {
					t.Fatalf("an unreadable diff kept the sandbox's files: %+v %+v", files, stored)
				}
				return
			}
			if files == nil || fmt.Sprint(*files) != fmt.Sprint(*test.want) || fmt.Sprint(stored) != fmt.Sprint(*test.want) {
				t.Fatalf("files %+v stored %+v, want %+v", files, stored, test.want)
			}
		})
	}
	var empty overload.ReviewResult
	if files := recordGateFiles(readableDiff, &empty); files == nil || empty.Metrics[gateFilesMetric] == nil {
		t.Fatalf("a result without metrics did not get the host's files: %+v", empty.Metrics)
	}
}

func TestFinishReviewGatesOnTheHostDiff(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	t.Setenv("OVERLOAD_ENABLE_POSTING", "")
	ctx := context.Background()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id := time.Now().UnixNano()
	installationID := id % 1_000_000_000_000
	repo := fmt.Sprintf("test-gate-%d/api", id)
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM run_artifacts WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM github_installations WHERE id=$1`, installationID)
	})
	if _, err := store.Pool.Exec(ctx, `INSERT INTO github_installations (id, account_login, account_type) VALUES ($1,'acme','Organization')`, installationID); err != nil {
		t.Fatal(err)
	}
	var repoID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name, enabled, dry_run, installation_id) VALUES ($1, true, false, $2) RETURNING id`, repo, installationID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	for index, test := range []struct {
		name, diff, wantState string
		wantFiles             bool
	}{
		{"readable diff passes", readableDiff, "success", true},
		{"unreadable diff fails the check", unreadableDiff, "failure", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var runID int64
			if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, dry_run, installation_id) VALUES ($1, $2, $3, $4, 'webhook', 'running', false, $5) RETURNING id`, repoID, index+1, sha, strings.Repeat("a", 40), installationID).Scan(&runID); err != nil {
				t.Fatal(err)
			}
			statuses := &fakeStatus{}
			worker := &ReviewWorker{Store: store, Status: statuses}
			result := overload.ReviewResult{Findings: []overload.Finding{}, Metrics: map[string]any{"routing": overload.Routing{Agents: []overload.AgentFiles{{Agent: "main", Files: []string{"a.go"}}}}, gateFilesMetric: sandboxClaim}}
			run := overload.Run{ID: runID, PRNumber: index + 1}
			spec := overload.ReviewSpec{Diff: test.diff, Workflow: overload.ResolvedWorkflow{Name: "w", Kind: "pr_review"}}
			if err := worker.finishReview(ctx, run, repo, spec, result, sha); err != nil {
				t.Fatal(err)
			}
			if len(statuses.states) != 1 || statuses.states[0] != test.wantState || !strings.Contains(statuses.descriptions[0], "not posted") {
				t.Fatalf("commit statuses %v %v, want %q", statuses.states, statuses.descriptions, test.wantState)
			}
			var metrics []byte
			if err := store.Pool.QueryRow(ctx, `SELECT metrics FROM runs WHERE id=$1`, runID).Scan(&metrics); err != nil {
				t.Fatal(err)
			}
			var stored map[string]json.RawMessage
			if err := json.Unmarshal(metrics, &stored); err != nil {
				t.Fatal(err)
			}
			if _, ok := stored[gateFilesMetric]; ok != test.wantFiles {
				t.Fatalf("stored gate files %s, want present=%v", stored[gateFilesMetric], test.wantFiles)
			}
			var gate string
			if err := store.Pool.QueryRow(ctx, `SELECT gate->>'state' FROM runs WHERE id=$1`, runID).Scan(&gate); err != nil || gate != test.wantState {
				t.Fatalf("recorded verdict %q %v", gate, err)
			}
		})
	}

	t.Run("a posted review queues a post job with bounded attempts", func(t *testing.T) {
		t.Setenv("OVERLOAD_ENABLE_POSTING", "1")
		var runID int64
		if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, dry_run, installation_id) VALUES ($1, 10, $2, $3, 'webhook', 'running', false, $4) RETURNING id`, repoID, sha, strings.Repeat("a", 40), installationID).Scan(&runID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = store.Pool.Exec(ctx, `DELETE FROM river_job WHERE kind = 'post_review' AND (args->>'run_id')::bigint = $1`, runID)
		})
		client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
		if err != nil {
			t.Fatal(err)
		}
		statuses := &fakeStatus{}
		worker := &ReviewWorker{Store: store, Status: statuses}
		result := overload.ReviewResult{Findings: []overload.Finding{}, Metrics: map[string]any{"routing": overload.Routing{Agents: []overload.AgentFiles{{Agent: "main", Files: []string{"a.go"}}}}}}
		spec := overload.ReviewSpec{Diff: readableDiff, Workflow: overload.ResolvedWorkflow{Name: "w", Kind: "pr_review"}}
		if err := worker.finishReview(rivertest.WorkContext(ctx, client), overload.Run{ID: runID, PRNumber: 10}, repo, spec, result, sha); err != nil {
			t.Fatal(err)
		}
		var attempts int
		if err := store.Pool.QueryRow(ctx, `SELECT max_attempts FROM river_job WHERE kind = 'post_review' AND (args->>'run_id')::bigint = $1`, runID).Scan(&attempts); err != nil || attempts != postgres.PostMaxAttempts {
			t.Fatalf("post job max_attempts %d (%v), want %d", attempts, err, postgres.PostMaxAttempts)
		}
		if len(statuses.states) != 0 {
			t.Fatalf("the review set a status before posting: %v", statuses.states)
		}
	})

	t.Run("an interrupted review reports an error status", func(t *testing.T) {
		var runID int64
		if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, dry_run, installation_id) VALUES ($1, 11, $2, $3, 'webhook', 'running', false, $4) RETURNING id`, repoID, sha, strings.Repeat("a", 40), installationID).Scan(&runID); err != nil {
			t.Fatal(err)
		}
		statuses := &fakeStatus{}
		worker := &ReviewWorker{Store: store, Status: statuses}
		if err := worker.Work(ctx, &river.Job[postgres.ReviewArgs]{Args: postgres.ReviewArgs{RunID: runID}}); err != nil {
			t.Fatal(err)
		}
		var status, code string
		if err := store.Pool.QueryRow(ctx, `SELECT status, error_code FROM runs WHERE id=$1`, runID).Scan(&status, &code); err != nil || status != "failed" || code != "review_interrupted" {
			t.Fatalf("run %q %q %v", status, code, err)
		}
		if fmt.Sprint(statuses.states) != "[error]" || !strings.Contains(statuses.descriptions[0], "Review interrupted") {
			t.Fatalf("commit statuses %v %v, want one error so the check does not stay pending", statuses.states, statuses.descriptions)
		}
	})
}
