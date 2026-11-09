package postgres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// APIKeyPrefix starts every overload API key, so secret scanners and people
// can recognize one.
const APIKeyPrefix = "ovl_"

// ErrInvalidAPIKey means a key is unknown, expired, revoked or belongs to a
// disabled user.
var ErrInvalidAPIKey = overload.ErrInvalidAPIKey

// CreateAPIKey creates a key for userID (0 for none) and returns it with
// the secret, which is shown once and never stored.
func (s *Store) CreateAPIKey(ctx context.Context, userID int64, name string, scopes []string, lifetime time.Duration) (overload.APIKey, string, error) {
	name = strings.TrimSpace(name)
	if err := overload.ValidateAPIKey(name, scopes, lifetime); err != nil {
		return overload.APIKey{}, "", err
	}
	var prefixBytes [6]byte
	var secretBytes [32]byte
	if _, err := rand.Read(prefixBytes[:]); err != nil {
		return overload.APIKey{}, "", err
	}
	if _, err := rand.Read(secretBytes[:]); err != nil {
		return overload.APIKey{}, "", err
	}
	prefix := APIKeyPrefix + hex.EncodeToString(prefixBytes[:])
	secret := prefix + "_" + base64.RawURLEncoding.EncodeToString(secretBytes[:])
	var owner *int64
	if userID != 0 {
		owner = &userID
	}
	key := overload.APIKey{UserID: userID, Name: name, Prefix: prefix, Scopes: scopes}
	err := s.Pool.QueryRow(ctx, `INSERT INTO api_keys (user_id, name, prefix, secret_hash, scopes, expires_at) VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6)) RETURNING id, expires_at, created_at`, owner, name, prefix, sessionHash(secret), scopes, lifetime.Seconds()).Scan(&key.ID, &key.ExpiresAt, &key.CreatedAt)
	if err != nil {
		return overload.APIKey{}, "", err
	}
	return key, secret, nil
}

// AuthenticateAPIKey returns the live key for secret and its owner (nil
// for keys without one), recording that it was used.
func (s *Store) AuthenticateAPIKey(ctx context.Context, secret string) (overload.APIKey, *overload.User, error) {
	if !strings.HasPrefix(secret, APIKeyPrefix) || len(secret) > 200 {
		return overload.APIKey{}, nil, ErrInvalidAPIKey
	}
	key, err := scanAPIKey(s.Pool.QueryRow(ctx, apiKeySelect+` WHERE k.secret_hash = $1 AND k.revoked_at IS NULL AND k.expires_at > now() AND (u.id IS NULL OR NOT u.disabled)`, sessionHash(secret)))
	if errors.Is(err, pgx.ErrNoRows) {
		return overload.APIKey{}, nil, ErrInvalidAPIKey
	}
	if err != nil {
		return overload.APIKey{}, nil, err
	}
	if key.LastUsedAt == nil || time.Since(*key.LastUsedAt) > time.Minute {
		if _, err := s.Pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, key.ID); err != nil {
			return overload.APIKey{}, nil, err
		}
	}
	if key.UserID == 0 {
		return key, nil, nil
	}
	var user overload.User
	err = s.Pool.QueryRow(ctx, `SELECT id, issuer, subject, email, name, role FROM users WHERE id = $1`, key.UserID).Scan(&user.ID, &user.Issuer, &user.Subject, &user.Email, &user.Name, &user.Role)
	if err != nil {
		return overload.APIKey{}, nil, err
	}
	return key, &user, nil
}

const apiKeySelect = `SELECT k.id, COALESCE(k.user_id, 0), COALESCE(u.email, ''), k.name, k.prefix, k.scopes, k.expires_at, k.created_at, k.last_used_at, k.revoked_at FROM api_keys k LEFT JOIN users u ON u.id = k.user_id`

func scanAPIKey(row pgx.Row) (overload.APIKey, error) {
	var key overload.APIKey
	err := row.Scan(&key.ID, &key.UserID, &key.UserEmail, &key.Name, &key.Prefix, &key.Scopes, &key.ExpiresAt, &key.CreatedAt, &key.LastUsedAt, &key.RevokedAt)
	return key, err
}

// ListAPIKeys returns at most overload.APIKeyListLimit keys, newest first:
// everyone's when all is set, otherwise userID's.
func (s *Store) ListAPIKeys(ctx context.Context, userID int64, all bool) ([]overload.APIKey, error) {
	rows, err := s.Pool.Query(ctx, apiKeySelect+` WHERE $2 OR COALESCE(k.user_id, 0) = $1 ORDER BY k.created_at DESC, k.id DESC LIMIT $3`, userID, all, overload.APIKeyListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []overload.APIKey
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// RevokeAPIKey revokes key id; unless anyOwner is set it must belong to
// userID.
func (s *Store) RevokeAPIKey(ctx context.Context, id, userID int64, anyOwner bool) error {
	command, err := s.Pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL AND ($3 OR COALESCE(user_id, 0) = $2)`, id, userID, anyOwner)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// RecordUsage stores one gateway call.
func (s *Store) RecordUsage(ctx context.Context, usage overload.Usage) error {
	var key, user *int64
	if usage.KeyID != 0 {
		key = &usage.KeyID
	}
	if usage.UserID != 0 {
		user = &usage.UserID
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO model_usage (api_key_id, user_id, model, endpoint, status, input_tokens, output_tokens, queue_ms, duration_ms) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, key, user, usage.Model, usage.Endpoint, usage.Status, usage.InputTokens, usage.OutputTokens, usage.QueueTime.Milliseconds(), usage.Duration.Milliseconds())
	return err
}

// UsageTotals sums gateway calls since since by person and model:
// everyone's when all is set, otherwise userID's.
func (s *Store) UsageTotals(ctx context.Context, since time.Time, userID int64, all bool) ([]overload.UsageTotal, error) {
	rows, err := s.Pool.Query(ctx, `SELECT COALESCE(u.email, ''), m.model, count(*), sum(m.input_tokens), sum(m.output_tokens), sum(m.duration_ms) / 60000.0 FROM model_usage m LEFT JOIN users u ON u.id = m.user_id
		WHERE m.created_at >= $1 AND ($3 OR COALESCE(m.user_id, 0) = $2) GROUP BY 1, 2 ORDER BY 1, 2`, since, userID, all)
	if err != nil {
		return nil, fmt.Errorf("usage totals: %w", err)
	}
	defer rows.Close()
	var totals []overload.UsageTotal
	for rows.Next() {
		var total overload.UsageTotal
		if err := rows.Scan(&total.UserEmail, &total.Model, &total.Calls, &total.InputTokens, &total.OutputTokens, &total.Minutes); err != nil {
			return nil, err
		}
		totals = append(totals, total)
	}
	return totals, rows.Err()
}
