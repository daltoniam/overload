package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// ErrUnknownForge is returned for a git host connection that does not exist.
var ErrUnknownForge = errors.New("unknown git host")

// SaveForge adds a GitLab or Forgejo connection, or updates the one with
// the same name. An update without a token keeps the stored token only
// when the server address is unchanged (ErrTokenRequired otherwise); the
// webhook secret is generated once and kept. A new connection gets the
// same catch-all pull request bindings GitHub has, so its repositories are
// reviewed by the same workflows until changed.
func (s *Store) SaveForge(ctx context.Context, connection overload.ForgeConnection) (overload.ForgeConnection, error) {
	base, err := overload.ForgeBaseURL(connection.BaseURL)
	if err != nil {
		return connection, err
	}
	connection.BaseURL = base
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return connection, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var existing overload.ForgeConnection
	err = tx.QueryRow(ctx, `SELECT id, kind, base_url, token, webhook_secret FROM forge_connections WHERE name=$1 FOR UPDATE`, connection.Name).Scan(&existing.ID, &existing.Kind, &existing.BaseURL, &existing.Token, &existing.WebhookSecret)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return connection, err
	}
	if existing.ID != 0 {
		if connection.Kind != existing.Kind {
			return connection, errors.New("a connection's kind cannot be changed; add a new connection instead")
		}
		if connection.Token == "" {
			if connection.Token, err = overload.ReusableToken(existing, connection); err != nil {
				return connection, err
			}
		}
		connection.ID, connection.WebhookSecret = existing.ID, existing.WebhookSecret
	}
	if err := connection.Validate(); err != nil {
		return connection, err
	}
	if existing.ID != 0 {
		if _, err := tx.Exec(ctx, `UPDATE forge_connections SET base_url=$2, token=$3, updated_at=now() WHERE id=$1`, connection.ID, connection.BaseURL, connection.Token); err != nil {
			return connection, err
		}
		return connection, tx.Commit(ctx)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return connection, err
	}
	connection.WebhookSecret = hex.EncodeToString(secret)
	if err := tx.QueryRow(ctx, `INSERT INTO forge_connections (name, kind, base_url, token, webhook_secret) VALUES ($1, $2, $3, $4, $5) RETURNING id`, connection.Name, connection.Kind, connection.BaseURL, connection.Token, connection.WebhookSecret).Scan(&connection.ID); err != nil {
		return connection, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO trigger_bindings (source, event, action, repository_full_name, workflow_id, enabled)
SELECT $1, event, action, '', workflow_id, enabled FROM trigger_bindings WHERE source = 'github' AND event = 'pull_request' AND repository_full_name = ''
ON CONFLICT (source, event, action, repository_full_name) DO NOTHING`, connection.Kind); err != nil {
		return connection, fmt.Errorf("copy bindings: %w", err)
	}
	return connection, tx.Commit(ctx)
}

// ListForges returns the connections with their webhook secrets but
// never their tokens.
func (s *Store) ListForges(ctx context.Context) ([]overload.ForgeConnection, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, name, kind, base_url, webhook_secret FROM forge_connections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var connections []overload.ForgeConnection
	for rows.Next() {
		var connection overload.ForgeConnection
		if err := rows.Scan(&connection.ID, &connection.Name, &connection.Kind, &connection.BaseURL, &connection.WebhookSecret); err != nil {
			return nil, err
		}
		connections = append(connections, connection)
	}
	return connections, rows.Err()
}

// LoadForge returns a connection with its token, for building a client.
func (s *Store) LoadForge(ctx context.Context, id int64) (overload.ForgeConnection, error) {
	connection := overload.ForgeConnection{ID: id}
	err := s.Pool.QueryRow(ctx, `SELECT name, kind, base_url, token, webhook_secret FROM forge_connections WHERE id=$1`, id).Scan(&connection.Name, &connection.Kind, &connection.BaseURL, &connection.Token, &connection.WebhookSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return connection, ErrUnknownForge
	}
	return connection, err
}

// LoadForgeByName returns a connection with its token by name.
func (s *Store) LoadForgeByName(ctx context.Context, name string) (overload.ForgeConnection, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `SELECT id FROM forge_connections WHERE name=$1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return overload.ForgeConnection{}, ErrUnknownForge
	}
	if err != nil {
		return overload.ForgeConnection{}, err
	}
	return s.LoadForge(ctx, id)
}

// DeleteForge removes a connection that no repository uses.
func (s *Store) DeleteForge(ctx context.Context, id int64) error {
	var used bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repositories WHERE forge_id=$1) OR EXISTS(SELECT 1 FROM runs WHERE forge_id=$1)`, id).Scan(&used); err != nil {
		return err
	}
	if used {
		return errors.New("git host has repositories or runs; remove its repositories first")
	}
	command, err := s.Pool.Exec(ctx, `DELETE FROM forge_connections WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrUnknownForge
	}
	return nil
}
