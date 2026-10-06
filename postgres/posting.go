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
	Findings       []overload.Finding
	Duplicates     []int64
	// Partial is set when the review completed degraded, for example
	// because a sub-agent failed.
	Partial bool
	// RepositoryPaused is set when the repository was disabled or put in
	// dry-run mode after the review was queued; nothing is posted.
	RepositoryPaused bool
	// Superseded is set when a newer commit of the pull request has its own
	// review, so comments on this commit would be stale.
	Superseded bool
}

var ErrNothingToPost = errors.New("run is not awaiting posting")

// LoadPostTarget returns the run's pending findings, separating those whose
// fingerprint was already posted on the same pull request.
func (s *Store) LoadPostTarget(ctx context.Context, runID int64) (PostTarget, error) {
	target := PostTarget{RunID: runID}
	var installation *int64
	err := s.Pool.QueryRow(ctx, `SELECT repo.full_name, r.pr_number, r.head_sha, r.installation_id, COALESCE(jsonb_array_length(CASE WHEN jsonb_typeof(r.metrics->'routing'->'degraded') = 'array' THEN r.metrics->'routing'->'degraded' END), 0) > 0, NOT repo.enabled OR repo.dry_run, EXISTS (SELECT 1 FROM runs n WHERE n.repository_id = r.repository_id AND n.pr_number = r.pr_number AND n.id > r.id AND n.head_sha <> r.head_sha AND n.status IN ('queued', 'running', 'completed')) FROM runs r JOIN repositories repo ON repo.id = r.repository_id WHERE r.id = $1 AND r.status = 'completed' AND NOT r.dry_run AND r.posted_at IS NULL AND r.post_status = 'queued'`, runID).Scan(&target.Repository, &target.PRNumber, &target.HeadSHA, &installation, &target.Partial, &target.RepositoryPaused, &target.Superseded)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, ErrNothingToPost
	}
	if err != nil {
		return target, err
	}
	if installation != nil {
		target.InstallationID = *installation
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
		} else {
			target.Findings = append(target.Findings, finding)
		}
	}
	return target, rows.Err()
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
