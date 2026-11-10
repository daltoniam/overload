package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/daltoniam/overload"
	"strings"
	"time"

	"github.com/daltoniam/overload/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// ErrReviewNotQueued explains why a manual review did not start; its text
// is safe to show to the person who asked.
var ErrReviewNotQueued = errors.New("review not queued")

// QueueManualReview queues a review of a pull request's current head as if
// it had just been opened, for pull requests opened before overload was
// watching or to review again on demand. It returns the run ID. Every rule
// a webhook review follows still applies: the repository must be enabled
// and installed, an "opened" binding must exist, and a head that already
// has a review is not reviewed twice.
//
// again reviews a head that already has a completed review (for example
// after changing the workflow); a review still queued or running is never
// duplicated.
func QueueManualReview(ctx context.Context, store *postgres.Store, client *river.Client[pgx.Tx], reader PullRequestReader, repository string, number int, again bool) (int64, error) {
	return QueueManualReviewOn(ctx, store, client, reader, "", repository, number, again)
}

// QueueManualReviewOn is QueueManualReview for a repository on the named
// GitLab or Forgejo connection ("" is GitHub).
func QueueManualReviewOn(ctx context.Context, store *postgres.Store, client *river.Client[pgx.Tx], reader PullRequestReader, forge, repository string, number int, again bool) (int64, error) {
	if number < 1 {
		return 0, fmt.Errorf("%w: pull request number must be positive", ErrReviewNotQueued)
	}
	repos, err := store.ListRepositories(ctx)
	if err != nil {
		return 0, err
	}
	var selected overload.Repository
	found := false
	for _, repo := range repos {
		if strings.EqualFold(repo.FullName, repository) && repo.Forge == forge {
			selected, repository, found = repo, repo.FullName, true
			if !repo.Enabled {
				return 0, fmt.Errorf("%w: %s is not enabled", ErrReviewNotQueued, repository)
			}
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("%w: %s is not a known repository", ErrReviewNotQueued, repository)
	}
	if selected.ForgeID == 0 && selected.InstallationID == 0 {
		return 0, fmt.Errorf("%w: %s is not linked to the GitHub App", ErrReviewNotQueued, repository)
	}
	pr, err := reader.PullRequest(ctx, selected, number)
	if err != nil {
		return 0, fmt.Errorf("read pull request: %w", err)
	}
	switch {
	case pr.State != "open":
		return 0, fmt.Errorf("%w: #%d is %s", ErrReviewNotQueued, number, pr.State)
	case pr.Draft:
		return 0, fmt.Errorf("%w: #%d is a draft", ErrReviewNotQueued, number)
	}
	head, base := pr.HeadSHA, pr.BaseSHA
	payload, err := json.Marshal(map[string]any{"manual": true, "repository": repository, "number": number, "head_sha": head, "requested_at": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return 0, err
	}
	delivery := postgres.PullRequestDelivery{
		DeliveryID:     fmt.Sprintf("manual-%s-%d-%s-%d", repository, number, head, time.Now().UnixNano()),
		Action:         postgres.ManualReviewAction,
		RepoName:       repository,
		RepoID:         pr.RepositoryID,
		InstallationID: selected.InstallationID,
		ForgeID:        selected.ForgeID,
		Forge:          selected.ForgeKind,
		PR:             number,
		HeadSHA:        head,
		BaseSHA:        base,
		Eligible:       head != "" && base != "",
		Payload:        payload,
		Manual:         true,
		Again:          again,
	}
	if _, err := store.IngestPR(ctx, client, delivery); err != nil {
		return 0, err
	}
	outcome, reason, runID, err := store.DeliveryOutcome(ctx, "manual", delivery.DeliveryID)
	if err != nil {
		return 0, err
	}
	if outcome != "queued" {
		if runID > 0 && reason == "head already reviewed" {
			if again {
				return runID, fmt.Errorf("%w: run %d for the current head of #%d is still queued or running", ErrReviewNotQueued, runID, number)
			}
			return runID, fmt.Errorf("%w: the current head of #%d already has run %d; choose to review again to run it anyway", ErrReviewNotQueued, number, runID)
		}
		return 0, fmt.Errorf("%w: %s", ErrReviewNotQueued, reason)
	}
	return runID, nil
}
