package queue

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
)

type fakePoster struct {
	reject    bool
	existing  int64
	posts     [][]overload.Finding
	events    []string
	summary   string
	standing  []int64
	dismissed []int64
}

func (poster *fakePoster) ChangesRequested(context.Context, int64, string, int) ([]int64, error) {
	return poster.standing, nil
}

func (poster *fakePoster) DismissReview(_ context.Context, _ int64, _ string, _ int, reviewID int64, message string) error {
	if message == "" {
		return fmt.Errorf("dismissal without a message")
	}
	poster.dismissed = append(poster.dismissed, reviewID)
	return nil
}

func (poster *fakePoster) FindReview(_ context.Context, installationID int64, _ string, _ int, marker string) (int64, error) {
	if installationID < 1 || !strings.HasPrefix(marker, "<!-- overload-run:") {
		return 0, fmt.Errorf("bad lookup")
	}
	return poster.existing, nil
}

func (poster *fakePoster) PostReview(_ context.Context, _ int64, _ string, _ int, _ string, event, summary string, findings []overload.Finding) (int64, error) {
	if poster.reject {
		return 0, fmt.Errorf("%w: commit_id is not part of the pull request", github.ErrReviewRejected)
	}
	poster.posts = append(poster.posts, findings)
	poster.events = append(poster.events, event)
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

	stale := newRun("completed", "queued", []overload.Finding{{Path: "g.go", Line: 1, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Old head", Body: "b", Confidence: 0.9, Evidence: "o()"}}, "pending")
	newer := newRun("running", "", nil, "pending")
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET head_sha=$2 WHERE id=$1`, newer, strings.Repeat("c", 40)); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{Args: postgres.PostReviewArgs{RunID: stale}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT post_status FROM runs WHERE id=$1`, stale).Scan(&postStatus); err != nil || postStatus != "superseded_by_newer_commit" || len(poster.posts) != 2 {
		t.Fatalf("posted a review of an older commit: %q %d %v", postStatus, len(poster.posts), err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET status='failed' WHERE id=$1`, newer); err != nil {
		t.Fatal(err)
	}

	poster.reject, poster.existing = true, 0
	rejected := newRun("completed", "queued", []overload.Finding{{Path: "f.go", Line: 1, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Stale", Body: "b", Confidence: 0.9, Evidence: "s()"}}, "pending")
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{Args: postgres.PostReviewArgs{RunID: rejected}}); err != nil {
		t.Fatalf("a rejected review must not be retried: %v", err)
	}
	var event string
	if err := store.Pool.QueryRow(ctx, `SELECT r.post_status, e.message FROM runs r JOIN run_events e ON e.run_id = r.id WHERE r.id=$1`, rejected).Scan(&postStatus, &event); err != nil || postStatus != "post_rejected" || !strings.Contains(event, "commit_id is not part") {
		t.Fatalf("rejection not recorded: %q %q %v", postStatus, event, err)
	}
	poster.reject = false

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

func TestPostWorkerReviewDecisions(t *testing.T) {
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
	repo := fmt.Sprintf("test-decision-%d/api", id)
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
	pr := 10
	newRun := func(decision string, findings ...overload.Finding) int64 {
		t.Helper()
		pr++
		snapshot := fmt.Sprintf(`{"review_decision":%q}`, decision)
		var runID int64
		if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, dry_run, post_status, installation_id, config_snapshot) VALUES ($1, $2, $3, $4, 'webhook', 'completed', false, 'queued', $5, $6) RETURNING id`, repoID, pr, strings.Repeat("b", 40), strings.Repeat("a", 40), installationID, snapshot).Scan(&runID); err != nil {
			t.Fatal(err)
		}
		tx, err := store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := postgres.InsertFindings(ctx, tx, runID, repo, pr, "pending", findings); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return runID
	}
	finding := func(severity string) overload.Finding {
		return overload.Finding{Path: "a.go", Line: 3, Side: "RIGHT", Severity: severity, Category: "bug", Title: severity + " bug", Body: "b", Confidence: 0.9, Evidence: "x()"}
	}
	work := func(poster *fakePoster, runID int64) string {
		t.Helper()
		if err := (&PostWorker{Store: store, Poster: poster}).Work(ctx, &river.Job[postgres.PostReviewArgs]{Args: postgres.PostReviewArgs{RunID: runID}}); err != nil {
			t.Fatal(err)
		}
		var status string
		if err := store.Pool.QueryRow(ctx, `SELECT post_status FROM runs WHERE id=$1`, runID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	tests := []struct {
		name          string
		decision      string
		findings      []overload.Finding
		standing      []int64
		wantStatus    string
		wantEvent     string
		wantDismissed []int64
	}{
		{"comment mode is unchanged", "", []overload.Finding{finding("critical")}, []int64{9}, "posted", overload.ReviewComment, nil},
		{"blocking finding requests changes", overload.ReviewDecisionRequestChanges, []overload.Finding{finding("high")}, []int64{9}, "posted", overload.ReviewRequestChanges, nil},
		{"minor finding comments and withdraws earlier request", overload.ReviewDecisionRequestChanges, []overload.Finding{finding("low")}, []int64{9}, "posted", overload.ReviewComment, []int64{9}},
		{"clean review withdraws earlier request", overload.ReviewDecisionRequestChanges, nil, []int64{9}, "nothing_new", "", []int64{9}},
		{"clean review approves", overload.ReviewDecisionApprove, nil, nil, "posted", overload.ReviewApprove, nil},
		{"approve with minor findings", overload.ReviewDecisionApprove, []overload.Finding{finding("medium")}, []int64{9}, "posted", overload.ReviewApprove, []int64{9}},
		{"approve mode still blocks", overload.ReviewDecisionApprove, []overload.Finding{finding("critical")}, nil, "posted", overload.ReviewRequestChanges, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			poster := &fakePoster{standing: tt.standing}
			status := work(poster, newRun(tt.decision, tt.findings...))
			if status != tt.wantStatus {
				t.Fatalf("status %q, want %q", status, tt.wantStatus)
			}
			if tt.wantEvent == "" && len(poster.events) != 0 || tt.wantEvent != "" && (len(poster.events) != 1 || poster.events[0] != tt.wantEvent) {
				t.Fatalf("events %v, want %q", poster.events, tt.wantEvent)
			}
			if len(poster.dismissed)+len(tt.wantDismissed) > 0 && fmt.Sprint(poster.dismissed) != fmt.Sprint(tt.wantDismissed) {
				t.Fatalf("dismissed %v, want %v", poster.dismissed, tt.wantDismissed)
			}
		})
	}

	t.Run("still-present blocking issue keeps the earlier request", func(t *testing.T) {
		first := newRun(overload.ReviewDecisionRequestChanges, finding("critical"))
		poster := &fakePoster{}
		if status := work(poster, first); status != "posted" || poster.events[0] != overload.ReviewRequestChanges {
			t.Fatalf("first review %q %v", status, poster.events)
		}
		pr--
		again := newRun(overload.ReviewDecisionRequestChanges, finding("critical"))
		poster = &fakePoster{standing: []int64{1001}}
		if status := work(poster, again); status != "nothing_new" || len(poster.events) != 0 || len(poster.dismissed) != 0 {
			t.Fatalf("repeat review %q events=%v dismissed=%v", status, poster.events, poster.dismissed)
		}
		pr--
		withdrawn := newRun(overload.ReviewDecisionRequestChanges, finding("critical"))
		poster = &fakePoster{}
		if status := work(poster, withdrawn); status != "posted" || len(poster.events) != 1 || poster.events[0] != overload.ReviewRequestChanges || len(poster.posts[0]) != 0 || !strings.Contains(poster.summary, "still present") {
			t.Fatalf("earlier request was dismissed by someone; expected a new request: %q %v %q", status, poster.events, poster.summary)
		}
	})
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
