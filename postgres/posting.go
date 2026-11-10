package postgres

import (
	"context"
	"errors"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

type PostReviewArgs struct {
	RunID int64 `json:"run_id"`
}

func (PostReviewArgs) Kind() string { return "post_review" }

// PostTarget is everything needed to post one run's findings.
type PostTarget struct {
	RunID          int64
	Repository     string
	PRNumber       int
	HeadSHA        string
	InstallationID int64
	// ForgeID is the GitLab or Forgejo connection to post to; 0 is GitHub.
	ForgeID    int64
	Findings   []overload.Finding
	Duplicates []int64
	// DuplicateSeverities are the severities of findings already posted on
	// an earlier commit and still present; they count toward the review
	// decision without being commented again.
	DuplicateSeverities []string
	// ReviewDecision and BlockSeverity come from the run's pinned workflow.
	ReviewDecision string
	BlockSeverity  string
	// Partial is set when the review completed degraded, for example
	// because a sub-agent failed.
	Partial bool
	// RepositoryPaused is set when the repository was disabled or put in
	// dry-run mode after the review was queued; nothing is posted.
	RepositoryPaused bool
	// Superseded is set when a newer commit of the pull request has its own
	// review, so comments on this commit would be stale.
	Superseded bool
	// Fixed are earlier findings this re-review found fixed, and
	// ResolveThreads whether their GitHub threads should be resolved.
	Fixed          []overload.Finding
	ResolveThreads bool
}

var ErrNothingToPost = errors.New("run is not awaiting posting")

// LoadPostTarget returns the run's pending findings, separating those whose
// fingerprint was already posted on the same pull request.
func (s *Store) LoadPostTarget(ctx context.Context, runID int64) (PostTarget, error) {
	target := PostTarget{RunID: runID}
	var installation *int64
	err := s.Pool.QueryRow(ctx, `SELECT repo.full_name, r.pr_number, r.head_sha, r.installation_id, COALESCE(r.forge_id, 0), COALESCE(jsonb_array_length(CASE WHEN jsonb_typeof(r.metrics->'routing'->'degraded') = 'array' THEN r.metrics->'routing'->'degraded' END), 0) > 0, NOT repo.enabled OR repo.dry_run, EXISTS (SELECT 1 FROM runs n WHERE n.repository_id = r.repository_id AND n.pr_number = r.pr_number AND n.id > r.id AND n.head_sha <> r.head_sha AND n.status IN ('queued', 'running', 'completed')), COALESCE(r.config_snapshot->>'review_decision', ''), COALESCE(r.config_snapshot->>'block_severity', '') FROM runs r JOIN repositories repo ON repo.id = r.repository_id WHERE r.id = $1 AND r.status = 'completed' AND NOT r.dry_run AND r.posted_at IS NULL AND r.post_status = 'queued'`, runID).Scan(&target.Repository, &target.PRNumber, &target.HeadSHA, &installation, &target.ForgeID, &target.Partial, &target.RepositoryPaused, &target.Superseded, &target.ReviewDecision, &target.BlockSeverity)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, ErrNothingToPost
	}
	if err != nil {
		return target, err
	}
	if installation != nil {
		target.InstallationID = *installation
	}
	// Earlier findings a re-review carried over (their file did not
	// change) or still sees count toward the decision, unless this run
	// found them again.
	carried, err := s.Pool.Query(ctx, `SELECT f.severity FROM runs r JOIN findings f ON f.id IN (SELECT value::bigint FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(r.metrics->'carried_finding_ids') = 'array' THEN r.metrics->'carried_finding_ids' ELSE '[]'::jsonb END)) JOIN runs fr ON fr.id = f.run_id WHERE r.id = $1 AND fr.repository_id = r.repository_id AND fr.pr_number = r.pr_number AND f.resolved_by_run_id IS NULL AND f.fingerprint NOT IN (SELECT fingerprint FROM findings WHERE run_id = $1)`, runID)
	if err != nil {
		return target, err
	}
	severities, err := pgx.CollectRows(carried, pgx.RowTo[string])
	if err != nil {
		return target, err
	}
	target.DuplicateSeverities = append(target.DuplicateSeverities, severities...)
	if err := s.Pool.QueryRow(ctx, `SELECT COALESCE((config_snapshot->'rereview'->>'resolve_threads')::boolean, false) FROM runs WHERE id = $1`, runID).Scan(&target.ResolveThreads); err != nil {
		return target, err
	}
	// One finding per fixed issue, preferring the copy that was posted
	// (it knows its GitHub comment).
	fixed, err := s.Pool.Query(ctx, `SELECT path, line, title, severity, comment_id FROM (SELECT DISTINCT ON (fingerprint) id, path, line, title, severity, COALESCE(github_comment_id, 0) AS comment_id FROM findings WHERE resolved_by_run_id = $1 ORDER BY fingerprint, github_comment_id IS NULL, id) fixed ORDER BY id`, runID)
	if err != nil {
		return target, err
	}
	target.Fixed, err = pgx.CollectRows(fixed, func(row pgx.CollectableRow) (overload.Finding, error) {
		var finding overload.Finding
		err := row.Scan(&finding.Path, &finding.Line, &finding.Title, &finding.Severity, &finding.CommentID)
		return finding, err
	})
	if err != nil {
		return target, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT f.id, f.path, f.line, f.side, f.severity, f.category, f.title, f.body, f.confidence, f.evidence,
EXISTS (SELECT 1 FROM findings p JOIN runs pr ON pr.id = p.run_id WHERE p.status = 'posted' AND p.fingerprint = f.fingerprint AND pr.repository_id = r.repository_id AND pr.pr_number = r.pr_number)
FROM findings f JOIN runs r ON r.id = f.run_id WHERE f.run_id = $1 AND f.status = 'pending' ORDER BY f.id`, runID)
	if err != nil {
		return target, err
	}
	defer rows.Close()
	for rows.Next() {
		var finding overload.Finding
		var duplicate bool
		if err := rows.Scan(&finding.ID, &finding.Path, &finding.Line, &finding.Side, &finding.Severity, &finding.Category, &finding.Title, &finding.Body, &finding.Confidence, &finding.Evidence, &duplicate); err != nil {
			return target, err
		}
		if duplicate {
			target.Duplicates = append(target.Duplicates, finding.ID)
			target.DuplicateSeverities = append(target.DuplicateSeverities, finding.Severity)
		} else {
			target.Findings = append(target.Findings, finding)
		}
	}
	return target, rows.Err()
}

// SetFindingComments records the GitHub review comment each posted finding
// became, by finding ID.
func (s *Store) SetFindingComments(ctx context.Context, comments map[int64]int64) error {
	for findingID, commentID := range comments {
		if _, err := s.Pool.Exec(ctx, `UPDATE findings SET github_comment_id = $2 WHERE id = $1`, findingID, commentID); err != nil {
			return err
		}
	}
	return nil
}

// FinishPost records the outcome of posting a run. reviewID is 0 when no
// review was created.
func (s *Store) FinishPost(ctx context.Context, target PostTarget, status string, reviewID int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var review *int64
	if reviewID > 0 {
		review = &reviewID
	}
	command, err := tx.Exec(ctx, `UPDATE runs SET post_status = $2, posted_at = now(), github_review_id = $3 WHERE id = $1 AND posted_at IS NULL`, target.RunID, status, review)
	if err != nil || command.RowsAffected() == 0 {
		return err
	}
	if reviewID > 0 {
		ids := make([]int64, 0, len(target.Findings))
		for _, finding := range target.Findings {
			ids = append(ids, finding.ID)
		}
		if _, err := tx.Exec(ctx, `UPDATE findings SET status = 'posted' WHERE id = ANY($1)`, ids); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE findings SET status = 'suppressed', suppressed_reason = 'already posted on this pull request' WHERE id = ANY($1)`, target.Duplicates); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'info', 'posted', $2)`, target.RunID, "Posting: "+status); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SkipPost marks a run that will not be posted, keeping its findings.
func (s *Store) SkipPost(ctx context.Context, runID int64, status string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE runs SET post_status = $2 WHERE id = $1 AND posted_at IS NULL`, runID, status)
	return err
}

// RejectPost records that GitHub refused the review, with GitHub's reason
// on the run's timeline. The findings stay on the run.
func (s *Store) RejectPost(ctx context.Context, runID int64, reason string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE runs SET post_status = 'post_rejected' WHERE id = $1 AND posted_at IS NULL`, runID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'error', 'posted', $2)`, runID, overload.TruncateUTF8("Posting rejected by GitHub: "+reason, 500)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
