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
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type fakePoster struct {
	reject     bool
	existing   int64
	posts      [][]overload.Finding
	events     []string
	summary    string
	standing   []github.StandingReview
	dismissed  []int64
	dismissErr error
	findErr    error
}

type fakeStatus struct {
	states       []string
	shas         []string
	descriptions []string
}

func (status *fakeStatus) SetStatus(_ context.Context, installationID int64, _ string, sha, state, description, _ string) error {
	if installationID < 1 || description == "" {
		return fmt.Errorf("bad status")
	}
	status.states = append(status.states, state)
	status.shas = append(status.shas, sha)
	status.descriptions = append(status.descriptions, description)
	return nil
}

func (poster *fakePoster) StandingReviews(context.Context, int64, string, int) ([]github.StandingReview, error) {
	return poster.standing, nil
}

func (poster *fakePoster) DismissReview(_ context.Context, _ int64, _ string, _ int, reviewID int64, message string) error {
	if message == "" {
		return fmt.Errorf("dismissal without a message")
	}
	if poster.dismissErr != nil {
		return poster.dismissErr
	}
	poster.dismissed = append(poster.dismissed, reviewID)
	return nil
}

func (poster *fakePoster) FindReview(_ context.Context, installationID int64, _ string, _ int, marker string) (int64, error) {
	if installationID < 1 || !strings.HasPrefix(marker, "<!-- overload-run:") {
		return 0, fmt.Errorf("bad lookup")
	}
	if poster.findErr != nil {
		return 0, poster.findErr
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
	job := &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: runID}}
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
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: crashed}}); err != nil {
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
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: partial}}); err != nil {
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
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: stale}}); err != nil {
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
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: rejected}}); err != nil {
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
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: paused}}); err != nil {
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
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: disabled}}); err != nil {
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
	insertRun := func(number int, trigger, snapshot, metrics string, findings ...overload.Finding) int64 {
		t.Helper()
		var runID int64
		if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, dry_run, post_status, installation_id, config_snapshot, metrics) VALUES ($1, $2, $3, $4, $5, 'completed', false, 'queued', $6, $7, $8) RETURNING id`, repoID, number, strings.Repeat("b", 40), strings.Repeat("a", 40), trigger, installationID, snapshot, metrics).Scan(&runID); err != nil {
			t.Fatal(err)
		}
		tx, err := store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := postgres.InsertFindings(ctx, tx, runID, repo, number, "pending", findings); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return runID
	}
	newGatedRun := func(snapshot, metrics string, findings ...overload.Finding) int64 {
		t.Helper()
		pr++
		return insertRun(pr, postgres.TriggerWebhook, snapshot, metrics, findings...)
	}
	// reviewedA is a complete review of a pull request changing only a.go.
	const reviewedA = `{"routing":{"agents":[{"agent":"main","files":["a.go"]}]},"gate_files":{"changed":["a.go"],"commentable":["a.go"]}}`
	newRun := func(decision string, findings ...overload.Finding) int64 {
		t.Helper()
		return newGatedRun(fmt.Sprintf(`{"review_decision":%q}`, decision), reviewedA, findings...)
	}
	finding := func(severity string) overload.Finding {
		return overload.Finding{Path: "a.go", Line: 3, Side: "RIGHT", Severity: severity, Category: "bug", Title: severity + " bug", Body: "b", Confidence: 0.9, Evidence: "x()"}
	}
	requested := func(id int64) github.StandingReview {
		return github.StandingReview{ID: id, State: github.ReviewStateChangesRequested}
	}
	approved := func(id int64) github.StandingReview {
		return github.StandingReview{ID: id, State: github.ReviewStateApproved}
	}
	statuses := &fakeStatus{}
	work := func(poster *fakePoster, runID int64) string {
		t.Helper()
		if err := (&PostWorker{Store: store, Poster: poster, Status: statuses}).Work(ctx, &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: runID}}); err != nil {
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
		standing      []github.StandingReview
		wantStatus    string
		wantEvent     string
		wantDismissed []int64
		wantState     string
	}{
		{"comment mode is unchanged", "", []overload.Finding{finding("critical")}, []github.StandingReview{requested(9)}, "posted", overload.ReviewComment, nil, "success"},
		{"comment mode withdraws a stale approval", "", []overload.Finding{finding("low")}, []github.StandingReview{requested(8), approved(9)}, "posted", overload.ReviewComment, []int64{9}, "success"},
		{"blocking finding requests changes", overload.ReviewDecisionRequestChanges, []overload.Finding{finding("high")}, []github.StandingReview{requested(9)}, "posted", overload.ReviewRequestChanges, nil, "failure"},
		{"minor finding comments and withdraws earlier request", overload.ReviewDecisionRequestChanges, []overload.Finding{finding("low")}, []github.StandingReview{requested(9)}, "posted", overload.ReviewComment, []int64{9}, "success"},
		{"clean review withdraws earlier request", overload.ReviewDecisionRequestChanges, nil, []github.StandingReview{requested(9)}, "nothing_new", "", []int64{9}, "success"},
		{"clean review approves", overload.ReviewDecisionApprove, nil, nil, "posted", overload.ReviewApprove, nil, "success"},
		{"approval keeps the earlier approval", overload.ReviewDecisionApprove, nil, []github.StandingReview{approved(9)}, "posted", overload.ReviewApprove, nil, "success"},
		{"approve with minor findings", overload.ReviewDecisionApprove, []overload.Finding{finding("medium")}, []github.StandingReview{requested(9)}, "posted", overload.ReviewApprove, []int64{9}, "success"},
		{"approve mode still blocks", overload.ReviewDecisionApprove, []overload.Finding{finding("critical")}, nil, "posted", overload.ReviewRequestChanges, nil, "failure"},
		{"blocking finding withdraws earlier approval", overload.ReviewDecisionApprove, []overload.Finding{finding("critical")}, []github.StandingReview{approved(9), requested(10)}, "posted", overload.ReviewRequestChanges, []int64{9}, "failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			poster := &fakePoster{standing: tt.standing}
			before := len(statuses.states)
			status := work(poster, newRun(tt.decision, tt.findings...))
			if len(statuses.states) != before+1 || statuses.states[before] != tt.wantState || statuses.shas[before] != strings.Repeat("b", 40) {
				t.Fatalf("commit statuses %v, want one %q", statuses.states[before:], tt.wantState)
			}
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

	gated := []struct {
		name, snapshot, metrics string
		standing                []github.StandingReview
		wantEvent, wantState    string
		wantSummary             string
		wantDismissed           []int64
	}{
		{"files not reviewed fail the check", `{"review_decision":"approve"}`, `{"degraded":true,"routing":{"too_large":["big.go"],"agents":[{"agent":"main","files":["a.go"]}],"degraded":["1 files were too large to review: big.go"]},"gate_files":{"changed":["a.go","big.go"],"commentable":["a.go","big.go"]}}`, nil, overload.ReviewComment, "failure", "Not reviewed", nil},
		{"a partial review withdraws an earlier approval", `{"review_decision":"approve"}`, `{"degraded":true,"routing":{"too_large":["big.go"],"agents":[{"agent":"main","files":["a.go"]}]},"gate_files":{"changed":["a.go","big.go"],"commentable":["a.go","big.go"]}}`, []github.StandingReview{approved(4)}, overload.ReviewComment, "failure", "`big.go`", []int64{4}},
		{"a failed agent fails the check", `{"review_decision":"request_changes"}`, `{"degraded":true,"routing":{"agents":[{"agent":"main","files":["a.go","b.go"],"failed":"timeout","unreviewed":["b.go"]}],"degraded":["main agent failed"]},"gate_files":{"changed":["a.go","b.go"],"commentable":["a.go","b.go"]}}`, nil, overload.ReviewComment, "failure", "`b.go`", nil},
		{"missing routing fails the check", `{"review_decision":"approve"}`, `{"gate_files":{"changed":["a.go"],"commentable":["a.go"]}}`, nil, overload.ReviewComment, "failure", "`a.go`", nil},
		{"a file missing from the routing fails the check", `{"review_decision":"approve"}`, `{"routing":{"agents":[{"agent":"main","files":["a.go"]}]},"gate_files":{"changed":["a.go","hidden.go"],"commentable":["a.go","hidden.go"]}}`, nil, overload.ReviewComment, "failure", "`hidden.go`", nil},
		{"unknown changed files fail the check", `{"review_decision":"approve"}`, `{"routing":{"agents":[{"agent":"main","files":["a.go"]}]}}`, nil, overload.ReviewComment, "failure", "", nil},
		{"human review paths block approval", `{"review_decision":"approve","human_review_paths":["auth/**"]}`, `{"routing":{"agents":[{"agent":"main","files":["auth/login.go"]}]},"gate_files":{"changed":["auth/login.go"],"commentable":["auth/login.go"]}}`, nil, overload.ReviewComment, "success", "human review paths", nil},
		{"a deleted file on a human path blocks approval", `{"review_decision":"approve","human_review_paths":["auth"]}`, `{"routing":{"uncommentable":["Auth/old.go"],"agents":[{"agent":"main","files":["a.go"]}]},"gate_files":{"changed":["a.go","Auth/old.go"],"commentable":["a.go"]}}`, nil, overload.ReviewComment, "success", "`Auth/old.go`", nil},
		{"approve paths limit approval", `{"review_decision":"approve","approve_paths":["docs/**"]}`, `{"routing":{"agents":[{"agent":"main","files":["docs/a.md","main.go"]}]},"gate_files":{"changed":["docs/a.md","main.go"],"commentable":["docs/a.md","main.go"]}}`, nil, overload.ReviewComment, "success", "", nil},
		{"a binary file outside approve paths blocks approval", `{"review_decision":"approve","approve_paths":["docs/**"]}`, `{"routing":{"uncommentable":["logo.png"],"agents":[{"agent":"main","files":["docs/a.md"]}]},"gate_files":{"changed":["docs/a.md","logo.png"],"commentable":["docs/a.md"]}}`, nil, overload.ReviewComment, "success", "", nil},
		{"skipped files must match approve paths", `{"review_decision":"approve","approve_paths":["docs/**"],"skip_paths":["go.sum"]}`, `{"routing":{"skipped":["go.sum"],"agents":[{"agent":"main","files":["docs/a.md"]}]},"gate_files":{"changed":["docs/a.md","go.sum"],"commentable":["docs/a.md","go.sum"]}}`, nil, overload.ReviewComment, "success", "", nil},
		{"a review of only skipped files never approves", `{"review_decision":"approve","skip_paths":["go.sum"]}`, `{"routing":{"skipped":["go.sum"],"agents":[{"agent":"main","files":[]}]},"gate_files":{"changed":["go.sum"],"commentable":["go.sum"]}}`, nil, overload.ReviewComment, "success", "", nil},
		{"approve paths allow approval", `{"review_decision":"approve","approve_paths":["docs/**","go.sum"],"skip_paths":["go.sum"]}`, `{"routing":{"skipped":["go.sum"],"agents":[{"agent":"main","files":["docs/a.md"]}]},"gate_files":{"changed":["docs/a.md","go.sum"],"commentable":["docs/a.md","go.sum"]}}`, nil, overload.ReviewApprove, "success", "", nil},
	}
	for _, tt := range gated {
		t.Run(tt.name, func(t *testing.T) {
			poster := &fakePoster{standing: tt.standing}
			before := len(statuses.states)
			work(poster, newGatedRun(tt.snapshot, tt.metrics, finding("low")))
			if len(statuses.states) != before+1 || statuses.states[before] != tt.wantState {
				t.Fatalf("commit statuses %v, want %q", statuses.states[before:], tt.wantState)
			}
			if len(poster.events) != 1 || poster.events[0] != tt.wantEvent {
				t.Fatalf("events %v, want %q", poster.events, tt.wantEvent)
			}
			if !strings.Contains(poster.summary, tt.wantSummary) {
				t.Fatalf("summary %q does not mention %q", poster.summary, tt.wantSummary)
			}
			if fmt.Sprint(poster.dismissed) != fmt.Sprint(tt.wantDismissed) && len(poster.dismissed)+len(tt.wantDismissed) > 0 {
				t.Fatalf("dismissed %v, want %v", poster.dismissed, tt.wantDismissed)
			}
		})
	}

	t.Run("unknown changed files are explained even with nothing new", func(t *testing.T) {
		poster := &fakePoster{}
		status := work(poster, newGatedRun(`{"review_decision":"approve"}`, `{"routing":{"agents":[{"agent":"main","files":["a.go"]}]}}`))
		if status != "posted" || len(poster.events) != 1 || poster.events[0] != overload.ReviewComment || !strings.Contains(poster.summary, "could not read this pull request's changed files") {
			t.Fatalf("status %q events %v summary %q", status, poster.events, poster.summary)
		}
	})

	t.Run("a failed withdrawal of an approval fails the check", func(t *testing.T) {
		poster := &fakePoster{standing: []github.StandingReview{approved(5)}, dismissErr: fmt.Errorf("403 dismissal restricted")}
		before := len(statuses.states)
		err := (&PostWorker{Store: store, Poster: poster, Status: statuses}).Work(ctx, &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: newRun(overload.ReviewDecisionApprove, finding("critical"))}})
		if err == nil || !strings.Contains(err.Error(), "dismiss earlier review") {
			t.Fatalf("withdrawal failure not reported for a retry: %v", err)
		}
		if fmt.Sprint(statuses.states[before:]) != "[failure]" || !strings.Contains(statuses.descriptions[before], "Could not withdraw overload's earlier approval") {
			t.Fatalf("commit statuses %v %v", statuses.states[before:], statuses.descriptions[before:])
		}
	})

	t.Run("an automated re-review cannot clear a failing verdict on the same commit", func(t *testing.T) {
		pr++
		number := pr
		snapshot := `{"review_decision":"approve"}`
		first := insertRun(number, postgres.TriggerWebhook, snapshot, reviewedA, finding("critical"))
		if status := work(&fakePoster{}, first); status != "posted" {
			t.Fatalf("first review %q", status)
		}
		before := len(statuses.states)
		poster := &fakePoster{standing: []github.StandingReview{requested(77)}}
		if status := work(poster, insertRun(number, postgres.TriggerAPI, snapshot, reviewedA)); status != "posted" {
			t.Fatalf("automated re-review %q", status)
		}
		if len(poster.events) != 1 || poster.events[0] != overload.ReviewComment || len(poster.dismissed) != 0 || !strings.Contains(poster.summary, "cannot clear that") {
			t.Fatalf("automated re-review approved or withdrew the request: events %v dismissed %v summary %q", poster.events, poster.dismissed, poster.summary)
		}
		if statuses.states[before] != "failure" || !strings.Contains(statuses.descriptions[before], "started by a person") {
			t.Fatalf("automated re-review cleared the status: %v %v", statuses.states[before:], statuses.descriptions[before:])
		}
		poster = &fakePoster{standing: []github.StandingReview{requested(77)}}
		if status := work(poster, insertRun(number, postgres.TriggerAPI, snapshot, reviewedA)); status != "nothing_new" || len(poster.events) != 0 || len(poster.dismissed) != 0 || statuses.states[len(statuses.states)-1] != "failure" {
			t.Fatalf("a held verdict must stay held for the next automated review, without explaining it again: %q events %v dismissed %v statuses %v", status, poster.events, poster.dismissed, statuses.states[before:])
		}
		poster = &fakePoster{standing: []github.StandingReview{requested(77)}}
		if status := work(poster, insertRun(number, postgres.TriggerManual, snapshot, reviewedA)); status != "posted" || len(poster.events) != 1 || poster.events[0] != overload.ReviewApprove || fmt.Sprint(poster.dismissed) != "[77]" || statuses.states[len(statuses.states)-1] != "success" {
			t.Fatalf("a person's re-review should clear the block: %q events %v dismissed %v statuses %v", status, poster.events, poster.dismissed, statuses.states[before:])
		}
		if status := work(&fakePoster{}, insertRun(number, postgres.TriggerAPI, snapshot, reviewedA)); status != "posted" || statuses.states[len(statuses.states)-1] != "success" {
			t.Fatalf("after a person cleared it, automation may pass again: %q %v", status, statuses.states[before:])
		}
	})

	t.Run("a post that keeps failing gives up on its last attempt", func(t *testing.T) {
		pr++
		runID := insertRun(pr, postgres.TriggerWebhook, `{"review_decision":"approve"}`, reviewedA)
		poster := &fakePoster{findErr: fmt.Errorf("GitHub is down")}
		worker := &PostWorker{Store: store, Poster: poster, Status: statuses}
		before := len(statuses.states)
		job := &river.Job[postgres.PostReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: postgres.PostMaxAttempts}, Args: postgres.PostReviewArgs{RunID: runID}}
		var postStatus, verdict string
		if err := worker.Work(ctx, job); err == nil {
			t.Fatal("a failed post was not retried")
		}
		if err := store.Pool.QueryRow(ctx, `SELECT post_status, COALESCE(gate->>'state', '') FROM runs WHERE id=$1`, runID).Scan(&postStatus, &verdict); err != nil || postStatus != "queued" || verdict != "" || len(statuses.states) != before {
			t.Fatalf("an early attempt gave up: %q %q %v %v", postStatus, verdict, statuses.states[before:], err)
		}
		job.Attempt = postgres.PostMaxAttempts
		if err := worker.Work(ctx, job); err == nil {
			t.Fatal("the last failure was hidden")
		}
		if err := store.Pool.QueryRow(ctx, `SELECT post_status, gate->>'state' FROM runs WHERE id=$1`, runID).Scan(&postStatus, &verdict); err != nil || postStatus != "post_failed" || verdict != "failure" {
			t.Fatalf("the last attempt left %q %q %v", postStatus, verdict, err)
		}
		if fmt.Sprint(statuses.states[before:]) != "[failure]" || !strings.Contains(statuses.descriptions[before], "could not finish posting") {
			t.Fatalf("commit statuses %v %v", statuses.states[before:], statuses.descriptions[before:])
		}
	})

	t.Run("human review files are listed again only when they change", func(t *testing.T) {
		pr++
		number := pr
		snapshot := `{"review_decision":"approve","human_review_paths":["auth/**"]}`
		human := func(files ...string) string {
			list, _ := json.Marshal(files)
			return fmt.Sprintf(`{"routing":{"agents":[{"agent":"main","files":%s}]},"gate_files":{"changed":%s,"commentable":%s}}`, list, list, list)
		}
		poster := &fakePoster{}
		if status := work(poster, insertRun(number, postgres.TriggerWebhook, snapshot, human("auth/a.go"))); status != "posted" || !strings.Contains(poster.summary, "`auth/a.go`") {
			t.Fatalf("first review %q %q", status, poster.summary)
		}
		poster = &fakePoster{}
		if status := work(poster, insertRun(number, postgres.TriggerWebhook, snapshot, human("auth/a.go", "main.go"))); status != "nothing_new" || len(poster.events) != 0 {
			t.Fatalf("unchanged human review files were posted again: %q %v", status, poster.events)
		}
		poster = &fakePoster{}
		if status := work(poster, insertRun(number, postgres.TriggerWebhook, snapshot, human("auth/a.go", "auth/b.go"))); status != "posted" || !strings.Contains(poster.summary, "`auth/b.go`") {
			t.Fatalf("changed human review files were not posted: %q %q", status, poster.summary)
		}
	})

	t.Run("gate notes are posted even with nothing new", func(t *testing.T) {
		poster := &fakePoster{}
		status := work(poster, newGatedRun(`{"review_decision":"approve","human_review_paths":["auth/**"]}`, `{"routing":{"agents":[{"agent":"main","files":["auth/login.go"]}]},"gate_files":{"changed":["auth/login.go"],"commentable":["auth/login.go"]}}`))
		if status != "posted" || len(poster.events) != 1 || poster.events[0] != overload.ReviewComment || !strings.Contains(poster.summary, "`auth/login.go`") {
			t.Fatalf("status %q events %v summary %q", status, poster.events, poster.summary)
		}
	})

	t.Run("a review posted before a crash still withdraws and sets the status", func(t *testing.T) {
		poster := &fakePoster{existing: 777, standing: []github.StandingReview{approved(5), requested(777)}}
		before := len(statuses.states)
		status := work(poster, newRun(overload.ReviewDecisionApprove, finding("critical")))
		if status != "posted" || len(poster.events) != 0 || fmt.Sprint(poster.dismissed) != "[5]" {
			t.Fatalf("status %q events %v dismissed %v", status, poster.events, poster.dismissed)
		}
		if len(statuses.states) != before+1 || statuses.states[before] != "failure" {
			t.Fatalf("commit statuses %v, want failure", statuses.states[before:])
		}
	})

	t.Run("posting off still fails the check", func(t *testing.T) {
		t.Setenv("OVERLOAD_ENABLE_POSTING", "")
		before := len(statuses.states)
		approvedRun := work(&fakePoster{}, newRun(overload.ReviewDecisionApprove))
		unreviewed := work(&fakePoster{}, newGatedRun(`{"review_decision":"approve"}`, `{"routing":{"agents":[]},"gate_files":{"changed":["a.go"],"commentable":["a.go"]}}`))
		if approvedRun != "posting_disabled" || unreviewed != "posting_disabled" || fmt.Sprint(statuses.states[before:]) != "[success failure]" {
			t.Fatalf("post statuses %q %q, commit statuses %v", approvedRun, unreviewed, statuses.states[before:])
		}
		if description := statuses.descriptions[before]; !strings.HasPrefix(description, "Would approve") || !strings.Contains(description, "not posted") {
			t.Fatalf("description %q claims an approval that was not posted", description)
		}
	})

	t.Run("an older review of the same commit is not posted", func(t *testing.T) {
		older := newRun(overload.ReviewDecisionApprove)
		if _, err := store.Pool.Exec(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, dry_run, installation_id) SELECT repository_id, pr_number, head_sha, base_sha, 'manual', 'running', false, installation_id FROM runs WHERE id=$1`, older); err != nil {
			t.Fatal(err)
		}
		poster := &fakePoster{}
		before := len(statuses.states)
		if status := work(poster, older); status != "superseded_by_newer_run" || len(poster.events) != 0 || len(statuses.states) != before {
			t.Fatalf("status %q events %v commit statuses %v", status, poster.events, statuses.states[before:])
		}
	})

	t.Run("still-present blocking issue keeps the earlier request", func(t *testing.T) {
		first := newRun(overload.ReviewDecisionRequestChanges, finding("critical"))
		poster := &fakePoster{}
		if status := work(poster, first); status != "posted" || poster.events[0] != overload.ReviewRequestChanges {
			t.Fatalf("first review %q %v", status, poster.events)
		}
		pr--
		again := newRun(overload.ReviewDecisionRequestChanges, finding("critical"))
		poster = &fakePoster{standing: []github.StandingReview{requested(1001)}}
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

func TestStatusSkipsDryRunRepositories(t *testing.T) {
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
	id := time.Now().UnixNano()
	installationID := id % 1_000_000_000_000
	repo := fmt.Sprintf("test-status-%d/api", id)
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id=(SELECT id FROM repositories WHERE full_name=$1)`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM github_installations WHERE id=$1`, installationID)
	})
	if _, err := store.Pool.Exec(ctx, `INSERT INTO github_installations (id, account_login, account_type) VALUES ($1,'acme','Organization')`, installationID); err != nil {
		t.Fatal(err)
	}
	var repoID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name, enabled, dry_run, installation_id) VALUES ($1, true, true, $2) RETURNING id`, repo, installationID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	var runID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, dry_run, installation_id) VALUES ($1, 1, $2, $3, 'webhook', 'running', true, $4) RETURNING id`, repoID, strings.Repeat("b", 40), strings.Repeat("a", 40), installationID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	statuses := &fakeStatus{}
	reportStatus(ctx, store, statuses, runID, statusPending, "Reviewing")
	if len(statuses.states) != 0 {
		t.Fatalf("set a status on a dry-run repository: %v", statuses.states)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE repositories SET dry_run=false WHERE id=$1; `, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET dry_run=false WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	reportStatus(ctx, store, statuses, runID, statusPending, "Reviewing")
	if len(statuses.states) != 1 || statuses.states[0] != statusPending {
		t.Fatalf("statuses %v", statuses.states)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, status, dry_run, installation_id) VALUES ($1, 1, $2, $3, 'webhook', 'queued', false, $4)`, repoID, strings.Repeat("b", 40), strings.Repeat("a", 40), installationID); err != nil {
		t.Fatal(err)
	}
	reportStatus(ctx, store, statuses, runID, "success", "Approved")
	if len(statuses.states) != 1 {
		t.Fatalf("an older review of the same commit replaced the newer one's status: %v", statuses.states)
	}
}

func TestStatusDescriptionFitsGitHub(t *testing.T) {
	for _, test := range []struct {
		name, description, want string
	}{
		{"short", "No issues found", "No issues found"},
		{"exactly the limit", strings.Repeat("a", 140), strings.Repeat("a", 140)},
		{"too long", strings.Repeat("a", 141), strings.Repeat("a", 139) + "…"},
		{"counts characters, not bytes", strings.Repeat("é", 141), strings.Repeat("é", 139) + "…"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := statusDescription(test.description)
			if got != test.want || len([]rune(got)) > 140 {
				t.Fatalf("statusDescription = %q (%d characters)", got, len([]rune(got)))
			}
		})
	}
}

func TestCodeListKeepsPathsInert(t *testing.T) {
	for _, test := range []struct {
		name  string
		paths []string
		want  string
	}{
		{"plain", []string{"a.go", "b.go"}, "`a.go`, `b.go`"},
		{"backtick", []string{"a`b.go"}, "`a'b.go`"},
		{"newlines cannot start markdown", []string{"a\n\n# Approved\n.go"}, "`a??# Approved?.go`"},
		{"mentions are broken up", []string{"docs/@octocat.md"}, "`docs/@ octocat.md`"},
		{"long lists are cut", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}, "`1`, `2`, `3`, `4`, `5`, `6`, `7`, `8`, `9`, `10` and 2 more"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := codeList(test.paths); got != test.want {
				t.Fatalf("codeList = %q, want %q", got, test.want)
			}
		})
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
