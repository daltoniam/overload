package queue

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
)

type fakePoster struct {
	existing int64
	posts    [][]overload.Finding
	summary  string
}

func (poster *fakePoster) FindReview(_ context.Context, installationID int64, _ string, _ int, marker string) (int64, error) {
	if installationID < 1 || !strings.HasPrefix(marker, "<!-- overload-run:") {
		return 0, fmt.Errorf("bad lookup")
	}
	return poster.existing, nil
}

func (poster *fakePoster) PostReview(_ context.Context, _ int64, _ string, _ int, _ string, summary string, findings []overload.Finding) (int64, error) {
	poster.posts = append(poster.posts, findings)
	poster.summary = summary
	poster.existing = int64(1000 + len(poster.posts))
	return poster.existing, nil
}

func TestPostWorkerIsIdempotent(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	t.Setenv("OVERLOAD_ENABLE_POSTING", "1")
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
	repo := fmt.Sprintf("test-post-%d/api", id)
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM findings WHERE run_id IN (SELECT id FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1))`, repo)
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
	old := overload.Finding{Path: "a.go", Line: 3, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Old bug", Body: "b", Confidence: 0.9, Evidence: "x()"}
	fresh := overload.Finding{Path: "b.go", Line: 7, Side: "RIGHT", Severity: "medium", Category: "bug", Title: "New bug", Body: "b", Confidence: 0.8, Evidence: "y()"}
	newRun := func(status, postStatus string, findings []overload.Finding, findingStatus string) int64 {
		t.Helper()
		var runID int64
		if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, dry_run, post_status, installation_id) VALUES ($1, 9, $2, $3, 'webhook', $4, false, $5, $6) RETURNING id`, repoID, strings.Repeat("b", 40), strings.Repeat("a", 40), status, postStatus, installationID).Scan(&runID); err != nil {
			t.Fatal(err)
		}
		for _, finding := range findings {
			if _, err := store.Pool.Exec(ctx, `INSERT INTO findings (run_id,path,line,side,severity,category,title,body,confidence,evidence,status,fingerprint) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, runID, finding.Path, finding.Line, finding.Side, finding.Severity, finding.Category, finding.Title, finding.Body, finding.Confidence, finding.Evidence, findingStatus, overload.FindingFingerprint(repo, 9, finding)); err != nil {
				t.Fatal(err)
			}
		}
		return runID
	}
	newRun("completed", "posted", []overload.Finding{old}, "posted")
	moved := old
	moved.Line = 30
	runID := newRun("completed", "queued", []overload.Finding{moved, fresh}, "pending")

	poster := &fakePoster{}
	worker := &PostWorker{Store: store, Poster: poster}
	job := &river.Job[postgres.PostReviewArgs]{Args: postgres.PostReviewArgs{RunID: runID}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	if len(poster.posts) != 1 || len(poster.posts[0]) != 1 || poster.posts[0][0].Title != "New bug" || !strings.Contains(poster.summary, fmt.Sprintf("overload-run:%d", runID)) {
		t.Fatalf("posts=%+v summary=%q", poster.posts, poster.summary)
	}
	if err := worker.Work(ctx, job); err != nil || len(poster.posts) != 1 {
		t.Fatalf("second run posted again: %d %v", len(poster.posts), err)
	}
	var postStatus string
	var reviewID int64
	if err := store.Pool.QueryRow(ctx, `SELECT post_status, github_review_id FROM runs WHERE id=$1`, runID).Scan(&postStatus, &reviewID); err != nil || postStatus != "posted" || reviewID != 1001 {
		t.Fatalf("status=%q review=%d err=%v", postStatus, reviewID, err)
	}
	var suppressed int
	_ = store.Pool.QueryRow(ctx, `SELECT count(*) FROM findings WHERE run_id=$1 AND status='suppressed'`, runID).Scan(&suppressed)
	if suppressed != 1 {
		t.Fatalf("duplicate not suppressed: %d", suppressed)
	}

	crashed := newRun("completed", "queued", []overload.Finding{{Path: "c.go", Line: 1, Side: "RIGHT", Severity: "low", Category: "bug", Title: "Third", Body: "b", Confidence: 0.7, Evidence: "z()"}}, "pending")
	poster.existing = 555
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{Args: postgres.PostReviewArgs{RunID: crashed}}); err != nil {
		t.Fatal(err)
	}
	if len(poster.posts) != 1 {
		t.Fatal("re-posted a review that GitHub already had")
	}
	if err := store.Pool.QueryRow(ctx, `SELECT github_review_id FROM runs WHERE id=$1`, crashed).Scan(&reviewID); err != nil || reviewID != 555 {
		t.Fatalf("existing review not recorded: %d %v", reviewID, err)
	}

	poster.existing = 0
	partial := newRun("completed", "queued", nil, "pending")
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dropped := overload.Finding{Path: "d.go", Line: 2, Side: "RIGHT", Severity: "low", Category: "bug", Title: "Dropped", Body: "b", Confidence: 0.5, Evidence: "w()", DropReason: "verifier: not real"}
	kept := overload.Finding{Path: "d.go", Line: 4, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Kept", Body: "b", Confidence: 0.9, Evidence: "v()"}
	if err := postgres.InsertFindings(ctx, tx, partial, repo, 9, "pending", []overload.Finding{kept, dropped}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET metrics='{"routing":{"agents":[],"degraded":["sub-agent sql failed"]}}' WHERE id=$1`, partial); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{Args: postgres.PostReviewArgs{RunID: partial}}); err != nil {
		t.Fatal(err)
	}
	if len(poster.posts) != 2 || len(poster.posts[1]) != 1 || poster.posts[1][0].Title != "Kept" || !strings.Contains(poster.summary, "This review is partial") || strings.Contains(poster.summary, "sql") {
		t.Fatalf("partial post=%+v summary=%q", poster.posts, poster.summary)
	}
	var droppedStatus, reason string
	if err := store.Pool.QueryRow(ctx, `SELECT status, suppressed_reason FROM findings WHERE run_id=$1 AND title='Dropped'`, partial).Scan(&droppedStatus, &reason); err != nil || droppedStatus != postgres.FindingDropped || reason != "verifier: not real" {
		t.Fatalf("dropped finding %q %q %v", droppedStatus, reason, err)
	}

	paused := newRun("completed", "queued", []overload.Finding{{Path: "e.go", Line: 1, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Paused", Body: "b", Confidence: 0.9, Evidence: "p()"}}, "pending")
	if _, err := store.Pool.Exec(ctx, `UPDATE repositories SET dry_run=true WHERE id=$1`, repoID); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{Args: postgres.PostReviewArgs{RunID: paused}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT post_status FROM runs WHERE id=$1`, paused).Scan(&postStatus); err != nil || postStatus != "repository_paused" || len(poster.posts) != 2 {
		t.Fatalf("posted to a repository switched to dry run: %q %d %v", postStatus, len(poster.posts), err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE repositories SET dry_run=false WHERE id=$1`, repoID); err != nil {
		t.Fatal(err)
	}

	t.Setenv("OVERLOAD_ENABLE_POSTING", "")
	disabled := newRun("completed", "queued", []overload.Finding{fresh}, "pending")
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{Args: postgres.PostReviewArgs{RunID: disabled}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT post_status FROM runs WHERE id=$1`, disabled).Scan(&postStatus); err != nil || postStatus != "posting_disabled" || len(poster.posts) != 2 {
		t.Fatalf("posted with switch off: %q %d", postStatus, len(poster.posts))
	}
}

func TestPlanPosting(t *testing.T) {
	for _, test := range []struct {
		dryRun, enabled bool
		want            postingPlan
	}{
		{true, true, postingPlan{findingStatus: "dry_run"}},
		{true, false, postingPlan{findingStatus: "dry_run"}},
		{false, true, postingPlan{findingStatus: "pending", postStatus: "queued", enqueue: true}},
		{false, false, postingPlan{findingStatus: "dry_run", postStatus: "posting_disabled"}},
	} {
		got := planPosting(test.dryRun, test.enabled)
		got.message = ""
		if got != test.want {
			t.Errorf("dryRun=%v enabled=%v: %+v, want %+v", test.dryRun, test.enabled, got, test.want)
		}
	}
}
