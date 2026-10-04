package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// PullRequestDelivery is a verified pull_request webhook, reduced to the
// fields ingest needs.
type PullRequestDelivery struct {
	DeliveryID     string
	Action         string
	RepoName       string
	RepoID         int64
	InstallationID int64
	PR             int
	HeadSHA        string
	BaseSHA        string
	Eligible       bool
	Payload        []byte
}

// repoFacts is what the database knows about the repository a delivery
// names. A zero value means no matching repository.
type repoFacts struct {
	found        bool
	id           int64
	enabled      bool
	dryRun       bool
	installation int64
	suspended    bool
}

const (
	skipNotEligible        = "event not eligible for review"
	skipNotEnabled         = "repository disabled or not installed"
	skipSuspended          = "installation suspended"
	skipInstallMismatch    = "installation does not match repository"
	skipNotLinked          = "repository not linked to this GitHub App installation"
	skipNoBinding          = "no matching workflow binding"
	skipAlreadyReviewed    = "head already reviewed"
	errMissingSHAs         = "reviewable PR requires head and base SHAs"
	deliveryOutcomeQueued  = "queued"
	deliveryOutcomeSkipped = "skipped"
)

// skipReason decides whether a delivery may start a review. It returns ""
// when the review may proceed. An App delivery must come from the
// installation the repository is linked to; a delivery without an
// installation (a plain repository webhook) only matches unlinked
// repositories, which are reviewed with the configured token.
func skipReason(delivery PullRequestDelivery, repo repoFacts) string {
	switch {
	case !delivery.Eligible:
		return skipNotEligible
	case !repo.found || !repo.enabled:
		return skipNotEnabled
	case repo.installation != 0 && repo.suspended:
		return skipSuspended
	case repo.installation != delivery.InstallationID && repo.installation != 0:
		return skipInstallMismatch
	case repo.installation == 0 && delivery.InstallationID != 0:
		return skipNotLinked
	}
	return ""
}

func lookupRepo(ctx context.Context, tx pgx.Tx, delivery PullRequestDelivery) (repoFacts, error) {
	const columns = `SELECT r.id, r.enabled, r.dry_run, COALESCE(r.installation_id, 0), COALESCE(i.suspended_at IS NOT NULL, false) FROM repositories r LEFT JOIN github_installations i ON i.id = r.installation_id `
	var facts repoFacts
	scan := func(row pgx.Row) error {
		err := row.Scan(&facts.id, &facts.enabled, &facts.dryRun, &facts.installation, &facts.suspended)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			facts.found = true
		}
		return err
	}
	if delivery.RepoID > 0 {
		if err := scan(tx.QueryRow(ctx, columns+`WHERE r.github_id = $1`, delivery.RepoID)); err != nil || facts.found {
			return facts, err
		}
	}
	// A repository added by hand has no GitHub ID yet; match it by name, but
	// never take over a row that belongs to a different GitHub repository.
	return facts, scan(tx.QueryRow(ctx, columns+`WHERE r.full_name = $1 AND r.github_id IS NULL`, delivery.RepoName))
}

// IngestPR records a pull_request delivery once and, when a repository and
// binding match, pins the workflow and queues a review in the same
// transaction. It reports false for a delivery ID it has already seen.
func (s *Store) IngestPR(ctx context.Context, client *river.Client[pgx.Tx], delivery PullRequestDelivery) (bool, error) {
	if delivery.DeliveryID == "" || !json.Valid(delivery.Payload) {
		return false, errors.New("invalid delivery")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var deliveryRow int64
	err = tx.QueryRow(ctx, `INSERT INTO webhook_deliveries (delivery_id, event, action, repository_full_name, payload, outcome) VALUES ($1, 'pull_request', $2, $3, $4, 'skipped') ON CONFLICT (source, delivery_id) DO NOTHING RETURNING id`, delivery.DeliveryID, delivery.Action, delivery.RepoName, delivery.Payload).Scan(&deliveryRow)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	skip := func(reason string, runID *int64) (bool, error) {
		if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET outcome = $1, skip_reason = $2, run_id = $3 WHERE id = $4`, deliveryOutcomeSkipped, reason, runID, deliveryRow); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	repo, err := lookupRepo(ctx, tx, delivery)
	if err != nil {
		return false, err
	}
	if reason := skipReason(delivery, repo); reason != "" {
		return skip(reason, nil)
	}
	if delivery.HeadSHA == "" || delivery.BaseSHA == "" {
		return false, errors.New(errMissingSHAs)
	}
	var bindingID int64
	var workflowName string
	err = tx.QueryRow(ctx, `SELECT b.id, w.name FROM trigger_bindings b JOIN workflows w ON w.id = b.workflow_id WHERE b.source = 'github' AND b.event = 'pull_request' AND b.action = $1 AND b.repository_full_name IN ('', $2) AND b.enabled AND w.enabled AND w.kind = 'pr_review' ORDER BY length(b.repository_full_name) DESC LIMIT 1`, delivery.Action, delivery.RepoName).Scan(&bindingID, &workflowName)
	if errors.Is(err, pgx.ErrNoRows) {
		return skip(skipNoBinding, nil)
	}
	if err != nil {
		return false, err
	}
	var existing int64
	err = tx.QueryRow(ctx, `SELECT id FROM runs WHERE repository_id = $1 AND pr_number = $2 AND head_sha = $3 AND status IN ('queued', 'running', 'completed') LIMIT 1`, repo.id, delivery.PR, delivery.HeadSHA).Scan(&existing)
	if err == nil {
		return skip(skipAlreadyReviewed, &existing)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	resolved, err := resolveWorkflow(ctx, tx, workflowName)
	if err != nil {
		return false, err
	}
	snapshot, err := json.Marshal(resolved)
	if err != nil {
		return false, err
	}
	if err := supersedeQueued(ctx, tx, client, repo.id, delivery.PR, delivery.HeadSHA); err != nil {
		return false, err
	}
	var installation *int64
	if repo.installation != 0 {
		installation = &repo.installation
	}
	var runID int64
	err = tx.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, dry_run, binding_id, config_snapshot, installation_id) VALUES ($1, $2, $3, $4, 'webhook', $5, $6, $7, $8) RETURNING id`, repo.id, delivery.PR, delivery.HeadSHA, delivery.BaseSHA, repo.dryRun, bindingID, snapshot, installation).Scan(&runID)
	if err != nil {
		return false, err
	}
	job, err := client.InsertTx(ctx, tx, ReviewArgs{RunID: runID}, nil)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET river_job_id = $1 WHERE id = $2`, job.Job.ID, runID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET outcome = $1, run_id = $2 WHERE id = $3`, deliveryOutcomeQueued, runID, deliveryRow); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'info', 'queued', 'Review queued')`, runID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// supersedeQueued cancels queued reviews of older heads of the same PR.
func supersedeQueued(ctx context.Context, tx pgx.Tx, client *river.Client[pgx.Tx], repoID int64, pr int, headSHA string) error {
	rows, err := tx.Query(ctx, `UPDATE runs SET status = 'superseded', finished_at = now() WHERE repository_id = $1 AND pr_number = $2 AND head_sha <> $3 AND status = 'queued' RETURNING river_job_id`, repoID, pr, headSHA)
	if err != nil {
		return err
	}
	jobIDs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		if _, err := client.JobCancelTx(ctx, tx, jobID); err != nil {
			return err
		}
	}
	return nil
}
