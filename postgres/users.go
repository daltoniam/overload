package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// ErrUserDisabled means an administrator disabled the user.
var ErrUserDisabled = errors.New("user disabled")

// ErrNoSession means the session cookie is unknown, expired or belongs to a
// disabled user.
var ErrNoSession = errors.New("no session")

// SignInUser records a sign-in: it creates the user or refreshes their
// email, name and role, and refuses disabled users.
func (s *Store) SignInUser(ctx context.Context, user overload.User) (overload.User, error) {
	if !overload.ValidRole(user.Role) || user.Issuer == "" || user.Subject == "" {
		return overload.User{}, errors.New("invalid user")
	}
	var disabled bool
	err := s.Pool.QueryRow(ctx, `INSERT INTO users (issuer, subject, email, name, role, last_login_at) VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (issuer, subject) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name, role = EXCLUDED.role, last_login_at = CASE WHEN users.disabled THEN users.last_login_at ELSE now() END
		RETURNING id, disabled, last_login_at`, user.Issuer, user.Subject, user.Email, user.Name, user.Role).Scan(&user.ID, &disabled, &user.LastLoginAt)
	if err != nil {
		return overload.User{}, err
	}
	if disabled {
		return overload.User{}, ErrUserDisabled
	}
	return user, nil
}

func sessionHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func randomToken() string {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

// CreateSession starts a session for the user and returns the cookie value.
func (s *Store) CreateSession(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	token := randomToken()
	if _, err := s.Pool.Exec(ctx, `DELETE FROM user_sessions WHERE expires_at < now()`); err != nil {
		return "", err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO user_sessions (token_hash, user_id, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3))`, sessionHash(token), userID, ttl.Seconds())
	return token, err
}

// SessionUser returns the user of a live session, ending sessions idle for
// longer than idle.
func (s *Store) SessionUser(ctx context.Context, token string, idle time.Duration) (overload.User, error) {
	var user overload.User
	var lastSeen time.Time
	err := s.Pool.QueryRow(ctx, `SELECT u.id, u.issuer, u.subject, u.email, u.name, u.role, u.last_login_at, s.last_seen_at FROM user_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now() AND NOT u.disabled`, sessionHash(token)).Scan(&user.ID, &user.Issuer, &user.Subject, &user.Email, &user.Name, &user.Role, &user.LastLoginAt, &lastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return overload.User{}, ErrNoSession
	}
	if err != nil {
		return overload.User{}, err
	}
	if time.Since(lastSeen) > idle {
		_, _ = s.Pool.Exec(ctx, `DELETE FROM user_sessions WHERE token_hash = $1`, sessionHash(token))
		return overload.User{}, ErrNoSession
	}
	if time.Since(lastSeen) > time.Minute {
		if _, err := s.Pool.Exec(ctx, `UPDATE user_sessions SET last_seen_at = now() WHERE token_hash = $1`, sessionHash(token)); err != nil {
			return overload.User{}, err
		}
	}
	return user, nil
}

// EndSession deletes a session.
func (s *Store) EndSession(ctx context.Context, token string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM user_sessions WHERE token_hash = $1`, sessionHash(token))
	return err
}

// ListUsers returns everyone who has signed in, by email.
func (s *Store) ListUsers(ctx context.Context) ([]overload.User, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, issuer, subject, email, name, role, disabled, last_login_at FROM users ORDER BY lower(email), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []overload.User
	for rows.Next() {
		var user overload.User
		if err := rows.Scan(&user.ID, &user.Issuer, &user.Subject, &user.Email, &user.Name, &user.Role, &user.Disabled, &user.LastLoginAt); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

// SetUserDisabled disables or re-enables a user. Disabling ends their
// sessions at once.
func (s *Store) SetUserDisabled(ctx context.Context, id int64, disabled bool) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `UPDATE users SET disabled = $2 WHERE id = $1`, id, disabled)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	if disabled {
		if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// OIDCLogin is a sign-in in progress.
type OIDCLogin struct {
	Nonce    string
	Verifier string
	Next     string
}

// StartLogin records a sign-in's state for ttl.
func (s *Store) StartLogin(ctx context.Context, state string, login OIDCLogin, ttl time.Duration) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM oidc_logins WHERE expires_at < now()`); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO oidc_logins (state_hash, nonce, verifier, next, expires_at) VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`, sessionHash(state), login.Nonce, login.Verifier, login.Next, ttl.Seconds())
	return err
}

// TakeLogin returns and deletes a sign-in's state, so a provider answer is
// accepted once.
func (s *Store) TakeLogin(ctx context.Context, state string) (OIDCLogin, error) {
	var login OIDCLogin
	err := s.Pool.QueryRow(ctx, `DELETE FROM oidc_logins WHERE state_hash = $1 AND expires_at > now() RETURNING nonce, verifier, next`, sessionHash(state)).Scan(&login.Nonce, &login.Verifier, &login.Next)
	if errors.Is(err, pgx.ErrNoRows) {
		return login, ErrNoSession
	}
	return login, err
}
