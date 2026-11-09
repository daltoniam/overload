package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/postgres/pgtest"
)

func TestUsersAndSessions(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	user, err := store.SignInUser(ctx, overload.User{Issuer: "https://idp", Subject: "1", Email: "a@example.com", Role: overload.RoleViewer})
	if err != nil || user.ID == 0 || user.LastLoginAt == nil {
		t.Fatalf("sign in: %+v %v", user, err)
	}
	again, err := store.SignInUser(ctx, overload.User{Issuer: "https://idp", Subject: "1", Email: "a@new.example.com", Role: overload.RoleAdmin})
	if err != nil || again.ID != user.ID {
		t.Fatalf("second sign in: %+v %v", again, err)
	}
	if _, err := store.SignInUser(ctx, overload.User{Issuer: "https://idp", Subject: "2", Role: "owner"}); err == nil {
		t.Fatal("accepted an unknown role")
	}
	token, err := store.CreateSession(ctx, user.ID, time.Hour, "id-token")
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.SessionUser(ctx, token, time.Hour)
	if err != nil || current.Role != overload.RoleAdmin || current.Email != "a@new.example.com" {
		t.Fatalf("session user: %+v %v", current, err)
	}
	if _, err := store.SessionUser(ctx, "unknown", time.Hour); !errors.Is(err, postgres.ErrNoSession) {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE user_sessions SET last_seen_at = now() - interval '3 hours'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionUser(ctx, token, 2*time.Hour); !errors.Is(err, postgres.ErrNoSession) {
		t.Fatalf("idle session: %v", err)
	}
	var kept int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions`).Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("idle session kept: %d %v", kept, err)
	}
	token, _ = store.CreateSession(ctx, user.ID, time.Hour, "")
	if _, err := store.Pool.Exec(ctx, `UPDATE user_sessions SET last_seen_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionUser(ctx, token, time.Hour); err != nil {
		t.Fatal(err)
	}
	var recent bool
	if err := store.Pool.QueryRow(ctx, `SELECT last_seen_at > now() - interval '1 minute' FROM user_sessions`).Scan(&recent); err != nil || !recent {
		t.Fatalf("session not marked seen: %v %v", recent, err)
	}
	if err := store.SetUserDisabled(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionUser(ctx, token, time.Hour); !errors.Is(err, postgres.ErrNoSession) {
		t.Fatalf("disabled user's session: %v", err)
	}
	if _, err := store.SignInUser(ctx, overload.User{Issuer: "https://idp", Subject: "1", Role: overload.RoleAdmin}); !errors.Is(err, postgres.ErrUserDisabled) {
		t.Fatalf("disabled sign in: %v", err)
	}
	if err := store.SetUserDisabled(ctx, user.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserDisabled(ctx, 999, true); !errors.Is(err, postgres.ErrUserNotFound) {
		t.Fatalf("disabled a missing user: %v", err)
	}
	token, _ = store.CreateSession(ctx, user.ID, time.Hour, "id-token")
	if hint, err := store.EndSession(ctx, token); err != nil || hint != "id-token" {
		t.Fatalf("end session: %q %v", hint, err)
	}
	if hint, err := store.EndSession(ctx, token); err != nil || hint != "" {
		t.Fatalf("ending an ended session: %q %v", hint, err)
	}
	if _, err := store.SessionUser(ctx, token, time.Hour); !errors.Is(err, postgres.ErrNoSession) {
		t.Fatal("ended session still works")
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO user_sessions (token_hash, user_id, expires_at) VALUES ('\x00', $1, now() - interval '1 minute'), ('\x01', $1, now() + interval '90 days')`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE user_sessions SET last_seen_at = now() - interval '31 days' WHERE token_hash = '\x01'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSession(ctx, user.ID, time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	var stale int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE token_hash IN ('\x00', '\x01')`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("expired or long-idle sessions kept: %d %v", stale, err)
	}
	users, err := store.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("users: %+v %v", users, err)
	}

	login := postgres.OIDCLogin{Nonce: "n", Verifier: "v", Next: "/runs"}
	if err := store.StartLogin(ctx, "state", login, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := store.TakeLogin(ctx, "state"); err != nil || got != login {
		t.Fatalf("take login: %+v %v", got, err)
	}
	if _, err := store.TakeLogin(ctx, "state"); !errors.Is(err, postgres.ErrNoSession) {
		t.Fatal("a login was accepted twice")
	}
	if err := store.StartLogin(ctx, "old", login, -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TakeLogin(ctx, "old"); !errors.Is(err, postgres.ErrNoSession) {
		t.Fatal("an expired login was accepted")
	}
}
