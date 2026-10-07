package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daltoniam/overload/github"
	"github.com/jackc/pgx/v5"
)

var ErrNoGitHubApp = errors.New("no GitHub App configured")

func (s *Store) SaveGitHubApp(ctx context.Context, app github.AppCredentials) error {
	if app.AppID < 1 || app.PrivateKey == "" || app.WebhookSecret == "" {
		return errors.New("incomplete GitHub App credentials")
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO github_app (id, app_id, slug, html_url, private_key, webhook_secret, owner_login, owner_type) VALUES (1, $1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET app_id=EXCLUDED.app_id, slug=EXCLUDED.slug, html_url=EXCLUDED.html_url, private_key=EXCLUDED.private_key, webhook_secret=EXCLUDED.webhook_secret, owner_login=EXCLUDED.owner_login, owner_type=EXCLUDED.owner_type, updated_at=now()`, app.AppID, app.Slug, app.HTMLURL, app.PrivateKey, app.WebhookSecret, app.OwnerLogin, app.OwnerType)
	return err
}

func (s *Store) LoadGitHubApp(ctx context.Context) (github.AppCredentials, error) {
	var app github.AppCredentials
	err := s.Pool.QueryRow(ctx, `SELECT app_id, slug, html_url, private_key, webhook_secret, owner_login, owner_type FROM github_app WHERE id = 1`).Scan(&app.AppID, &app.Slug, &app.HTMLURL, &app.PrivateKey, &app.WebhookSecret, &app.OwnerLogin, &app.OwnerType)
	if errors.Is(err, pgx.ErrNoRows) {
		return app, ErrNoGitHubApp
	}
	return app, err
}

// GitHubWebhookSecret returns the stored App webhook secret, or "" if none.
func (s *Store) GitHubWebhookSecret(ctx context.Context) (string, error) {
	app, err := s.LoadGitHubApp(ctx)
	if errors.Is(err, ErrNoGitHubApp) {
		return "", nil
	}
	return app.WebhookSecret, err
}

type GitHubInstallation struct {
	ID           int64
	AccountLogin string
	AccountType  string
	Suspended    bool
	Repositories int
}

func (s *Store) ListGitHubInstallations(ctx context.Context) ([]GitHubInstallation, error) {
	rows, err := s.Pool.Query(ctx, `SELECT i.id, i.account_login, i.account_type, i.suspended_at IS NOT NULL, count(r.id) FROM github_installations i LEFT JOIN repositories r ON r.installation_id = i.id GROUP BY i.id ORDER BY i.account_login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var installations []GitHubInstallation
	for rows.Next() {
		var installation GitHubInstallation
		if err := rows.Scan(&installation.ID, &installation.AccountLogin, &installation.AccountType, &installation.Suspended, &installation.Repositories); err != nil {
			return nil, err
		}
		installations = append(installations, installation)
	}
	return installations, rows.Err()
}

// syncRepository links a GitHub repository to an installation. Rows are
// keyed by GitHub repository ID, so a rename keeps the row and its settings,
// and a different repository that later reuses a name never inherits them.
// A row still holding the name for another repository is disabled and
// renamed out of the way.
func syncRepository(ctx context.Context, tx pgx.Tx, installationID int64, repo github.InstallationRepository) error {
	var existing int64
	err := tx.QueryRow(ctx, `SELECT id FROM repositories WHERE github_id = $1`, repo.ID).Scan(&existing)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if existing == 0 {
		var claimed int64
		err := tx.QueryRow(ctx, `UPDATE repositories SET github_id = $1, installation_id = $2, updated_at = now() WHERE full_name = $3 AND github_id IS NULL RETURNING id`, repo.ID, installationID, repo.FullName).Scan(&claimed)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE repositories SET full_name = full_name || ' (replaced ' || id || ')', enabled = false, installation_id = NULL, updated_at = now() WHERE full_name = $1 AND id <> $2`, repo.FullName, existing); err != nil {
		return err
	}
	if existing != 0 {
		_, err := tx.Exec(ctx, `UPDATE repositories SET full_name = $1, installation_id = $2, updated_at = now() WHERE id = $3`, repo.FullName, installationID, existing)
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO repositories (github_id, installation_id, full_name) VALUES ($1, $2, $3)`, repo.ID, installationID, repo.FullName)
	return err
}

// ApplyInstallationEvent records an installation or installation_repositories
// delivery once and syncs installations and repositories. New repositories
// start disabled and dry-run; removed or uninstalled ones are disabled.
func (s *Store) ApplyInstallationEvent(ctx context.Context, deliveryID, eventName string, payload []byte, event github.InstallationEvent) (bool, error) {
	if deliveryID == "" || !json.Valid(payload) {
		return false, errors.New("invalid delivery")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var deliveryRow int64
	err = tx.QueryRow(ctx, `INSERT INTO webhook_deliveries (delivery_id, event, action, payload, outcome) VALUES ($1, $2, $3, $4, 'applied') ON CONFLICT (source, delivery_id) DO NOTHING RETURNING id`, deliveryID, eventName, event.Action, payload).Scan(&deliveryRow)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	installation := event.Installation
	if _, err := tx.Exec(ctx, `INSERT INTO github_installations (id, account_login, account_type) VALUES ($1, $2, $3) ON CONFLICT (id) DO UPDATE SET account_login=EXCLUDED.account_login, account_type=EXCLUDED.account_type, updated_at=now()`, installation.ID, installation.Account.Login, installation.Account.Type); err != nil {
		return false, err
	}
	added, removed := event.RepositoriesAdded, event.RepositoriesRemoved
	switch eventName + "/" + event.Action {
	case "installation/created", "installation/new_permissions_accepted", "installation/unsuspend":
		added = event.Repositories
		if _, err := tx.Exec(ctx, `UPDATE github_installations SET suspended_at = NULL WHERE id = $1`, installation.ID); err != nil {
			return false, err
		}
	case "installation/suspend":
		if _, err := tx.Exec(ctx, `UPDATE github_installations SET suspended_at = now() WHERE id = $1`, installation.ID); err != nil {
			return false, err
		}
	case "installation/deleted":
		if _, err := tx.Exec(ctx, `UPDATE repositories SET installation_id = NULL, enabled = false, updated_at = now() WHERE installation_id = $1`, installation.ID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM github_installations WHERE id = $1`, installation.ID); err != nil {
			return false, err
		}
	case "installation_repositories/added", "installation_repositories/removed":
	default:
		if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET outcome = 'skipped', skip_reason = 'installation action not handled' WHERE id = $1`, deliveryRow); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	for _, repo := range added {
		if err := syncRepository(ctx, tx, installation.ID, repo); err != nil {
			return false, fmt.Errorf("sync %s: %w", repo.FullName, err)
		}
	}
	for _, repo := range removed {
		if _, err := tx.Exec(ctx, `UPDATE repositories SET installation_id = NULL, enabled = false, updated_at = now() WHERE github_id = $1 AND installation_id = $2`, repo.ID, installation.ID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}
