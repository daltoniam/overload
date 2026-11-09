package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/daltoniam/overload"
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
	// Manual marks a review someone asked for (the UI's "Review now" or
	// the CLI) instead of a GitHub webhook. It matches the repository's
	// "opened" binding and is recorded with the manual trigger.
	Manual bool
	// Again reviews a head that already has a completed review; a review
	// that is still queued or running is never duplicated.
	Again bool
}

// ManualReviewAction is the binding action a manual review uses: the same
// workflow as a newly opened pull request.
const ManualReviewAction = "opened"

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
	skipNotEligible     = "event not eligible for review"
	skipNotEnabled      = "repository disabled or not installed"
	skipSuspended       = "installation suspended"
	skipInstallMismatch = "installation does not match repository"
	skipNotLinked       = "repository not linked to this GitHub App installation"
	skipNoBinding       = "no matching workflow binding"
	skipAlreadyReviewed = "head already reviewed"
	skipWorkflowBroken  = "workflow cannot run"
	// SkipReviewLimit marks a push past the workflow's reviews per pull
	// request.
	SkipReviewLimit        = "review_limit_reached"
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
// transaction. It reports false for a delivery ID or payload it has already
// seen.
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
	digest := sha256.Sum256(delivery.Payload)
	source, trigger := "github", "webhook"
	if delivery.Manual {
		source, trigger = "manual", "manual"
	}
	err = tx.QueryRow(ctx, `INSERT INTO webhook_deliveries (delivery_id, event, action, repository_full_name, payload, outcome, payload_sha256, source) VALUES ($1, 'pull_request', $2, $3, $4, 'skipped', $5, $6) ON CONFLICT DO NOTHING RETURNING id`, delivery.DeliveryID, delivery.Action, delivery.RepoName, delivery.Payload, hex.EncodeToString(digest[:]), source).Scan(&deliveryRow)
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
	// Deliveries for one pull request are ingested one at a time, so two
	// that arrive together cannot both pass the "already reviewed" check.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 729150))`, fmt.Sprintf("%d/%d", repo.id, delivery.PR)); err != nil {
		return false, err
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
	statuses := []string{"queued", "running", "completed"}
	if delivery.Manual && delivery.Again {
		statuses = []string{"queued", "running"}
	}
	err = tx.QueryRow(ctx, `SELECT id FROM runs WHERE repository_id = $1 AND pr_number = $2 AND head_sha = $3 AND status = ANY($4) ORDER BY id DESC LIMIT 1`, repo.id, delivery.PR, delivery.HeadSHA, statuses).Scan(&existing)
	if err == nil {
		return skip(skipAlreadyReviewed, &existing)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	resolved, err := resolveWorkflow(ctx, tx, workflowName)
	if errors.Is(err, ErrWorkflowUnavailable) {
		return skip(overload.TruncateUTF8(skipWorkflowBroken+": "+strings.TrimPrefix(err.Error(), ErrWorkflowUnavailable.Error()+": "), 300), nil)
	}
	if err != nil {
		return false, err
	}
	snapshot, err := json.Marshal(resolved)
	if err != nil {
		return false, err
	}
	expected, err := expectedRunCost(ctx, tx, snapshot)
	if err != nil {
		return false, err
	}
	budget, err := s.checkBudget(ctx, tx, repo.id, expected)
	if err != nil {
		return false, err
	}
	if budget.Exceeded {
		if repo.installation != 0 {
			if _, err := client.InsertTx(ctx, tx, SkipStatusArgs{Repository: delivery.RepoName, InstallationID: repo.installation, SHA: delivery.HeadSHA, State: budget.SkipStatus, Description: budget.Description()}, nil); err != nil {
				return false, err
			}
		}
		return skip(budgetSkipReason(budget), nil)
	}
	if reason, err := reviewLimit(ctx, tx, client, resolved, delivery, repo); err != nil || reason != "" {
		if err != nil {
			return false, err
		}
		return skip(reason, nil)
	}
	if err := supersedeQueued(ctx, tx, client, repo.id, delivery.PR, delivery.HeadSHA); err != nil {
		return false, err
	}
	var installation *int64
	if repo.installation != 0 {
		installation = &repo.installation
	}
	var runID int64
	err = tx.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, head_sha, base_sha, trigger, dry_run, binding_id, config_snapshot, installation_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`, repo.id, delivery.PR, delivery.HeadSHA, delivery.BaseSHA, trigger, repo.dryRun, bindingID, snapshot, installation).Scan(&runID)
	if err != nil {
		return false, err
	}
	queuedMessage := "Review queued"
	if delivery.Manual {
		queuedMessage = "Review queued by hand"
	}
	var options *river.InsertOpts
	if wait := resolved.Rereview.Debounce(); wait > 0 && delivery.Action == "synchronize" && !delivery.Manual {
		// A push is held so a quick follow-up push supersedes it instead
		// of both being reviewed.
		options = &river.InsertOpts{ScheduledAt: s.now().Add(wait)}
		queuedMessage = fmt.Sprintf("Review queued; waiting %s for further pushes", wait)
	}
	job, err := client.InsertTx(ctx, tx, ReviewArgs{RunID: runID}, options)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET river_job_id = $1 WHERE id = $2`, job.Job.ID, runID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET outcome = $1, run_id = $2 WHERE id = $3`, deliveryOutcomeQueued, runID, deliveryRow); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'info', 'queued', $2)`, runID, queuedMessage); err != nil {
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

// DeliveryOutcome reports what became of a recorded delivery: its outcome,
// skip reason and run, if any.
func (s *Store) DeliveryOutcome(ctx context.Context, source, deliveryID string) (string, string, int64, error) {
	var outcome, reason string
	var runID *int64
	err := s.Pool.QueryRow(ctx, `SELECT outcome, COALESCE(skip_reason, ''), run_id FROM webhook_deliveries WHERE source = $1 AND delivery_id = $2`, source, deliveryID).Scan(&outcome, &reason, &runID)
	if err != nil {
		return "", "", 0, err
	}
	if runID == nil {
		return outcome, reason, 0, nil
	}
	return outcome, reason, *runID, nil
}

// KnownDelivery reports whether a GitHub webhook delivery was recorded.
func (s *Store) KnownDelivery(ctx context.Context, guid string) (bool, error) {
	var known bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM webhook_deliveries WHERE source = 'github' AND delivery_id = $1)`, guid).Scan(&known)
	return known, err
}

// budgetSkipReason explains a review the budget left out.
func budgetSkipReason(check BudgetCheck) string {
	scope := "global"
	if check.Scope != "" {
		scope = check.Scope
	}
	return fmt.Sprintf("%s: %s %s budget %s reached (%s spent, %s expected for queued and running reviews, %s for this one)", SkipBudgetExhausted, scope, check.Period, overload.FormatUSD(check.Cap), overload.FormatUSD(check.Spent), overload.FormatUSD(check.Pending), overload.FormatUSD(check.Expected))
}

// reviewLimit returns a skip reason when a webhook review would pass the
// workflow's reviews per pull request, and sets the head's commit status
// so a required check does not wait. Reviews asked for by hand always run.
func reviewLimit(ctx context.Context, tx pgx.Tx, client *river.Client[pgx.Tx], workflow overload.ResolvedWorkflow, delivery PullRequestDelivery, repo repoFacts) (string, error) {
	limit := workflow.Rereview.ReviewLimit()
	if limit == 0 || delivery.Manual {
		return "", nil
	}
	var reviews int
	// Queued reviews of other heads are about to be superseded by this
	// one, so they do not count.
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE repository_id=$1 AND pr_number=$2 AND trigger='webhook' AND (status IN ('running', 'completed') OR (status = 'queued' AND head_sha = $3))`, repo.id, delivery.PR, delivery.HeadSHA).Scan(&reviews); err != nil {
		return "", err
	}
	if reviews < limit {
		return "", nil
	}
	if repo.installation != 0 {
		description := fmt.Sprintf("Overload: review limit reached (%d); comment /overload review to review again", limit)
		if _, err := client.InsertTx(ctx, tx, SkipStatusArgs{Repository: delivery.RepoName, InstallationID: repo.installation, SHA: delivery.HeadSHA, State: BudgetSkipSuccess, Description: description}, nil); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%s: %s of this pull request already", SkipReviewLimit, overload.Plural(reviews, "webhook review")), nil
}
