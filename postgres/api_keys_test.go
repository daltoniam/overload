package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/postgres/pgtest"
)

func TestAPIKeysAndUsage(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	user, err := store.SignInUser(ctx, overload.User{Issuer: "https://idp", Subject: "1", Email: "a@example.com", Role: overload.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	key, secret, err := store.CreateAPIKey(ctx, user.ID, " laptop ", []string{overload.ScopeModels}, 30*24*time.Hour)
	if err != nil || !strings.HasPrefix(secret, key.Prefix+"_") || key.Name != "laptop" {
		t.Fatalf("create: %+v %q %v", key, secret, err)
	}
	for _, scope := range []string{"admin", ""} {
		if _, _, err := store.CreateAPIKey(ctx, user.ID, "x", []string{scope}, time.Hour); err == nil {
			t.Fatalf("created a key with the %s scope", scope)
		}
	}
	shared, sharedSecret, err := store.CreateAPIKey(ctx, 0, "ci", []string{overload.ScopeModels}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	found, owner, err := store.AuthenticateAPIKey(ctx, secret)
	if err != nil || found.ID != key.ID || owner == nil || owner.Email != "a@example.com" || !found.HasScope(overload.ScopeModels) {
		t.Fatalf("authenticate: %+v %+v %v", found, owner, err)
	}
	if found, owner, err := store.AuthenticateAPIKey(ctx, sharedSecret); err != nil || owner != nil || found.ID != shared.ID || found.State(time.Now()) != overload.APIKeyActive {
		t.Fatalf("shared key: %+v %v", owner, err)
	}
	for _, bad := range []string{"", "ovl_nope", secret + "x", strings.Repeat("ovl_", 60)} {
		if _, _, err := store.AuthenticateAPIKey(ctx, bad); !errors.Is(err, postgres.ErrInvalidAPIKey) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	mine, err := store.ListAPIKeys(ctx, user.ID, false)
	if err != nil || len(mine) != 1 || mine[0].LastUsedAt == nil {
		t.Fatalf("own keys: %+v %v", mine, err)
	}
	all, err := store.ListAPIKeys(ctx, 0, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("all keys: %+v %v", all, err)
	}
	if err := store.RevokeAPIKey(ctx, shared.ID, user.ID, false); !errors.Is(err, overload.ErrInvalidAPIKey) {
		t.Fatalf("revoked someone else's key: %v", err)
	}
	if got, err := store.GetAPIKey(ctx, key.ID, user.ID, false); err != nil || got.Name != "laptop" {
		t.Fatalf("own key: %+v %v", got, err)
	}
	if _, err := store.GetAPIKey(ctx, shared.ID, user.ID, false); !errors.Is(err, overload.ErrInvalidAPIKey) {
		t.Fatalf("read someone else's key: %v", err)
	}
	if got, err := store.GetAPIKey(ctx, shared.ID, 0, true); err != nil || got.Name != "ci" {
		t.Fatalf("admin read: %+v %v", got, err)
	}
	if err := store.RecordUsage(ctx, overload.Usage{KeyID: key.ID, UserID: user.ID, Model: "flint", Endpoint: "/v1/chat/completions", Status: 200, InputTokens: 10, OutputTokens: 4, Duration: 3 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	totals, err := store.UsageTotals(ctx, time.Now().Add(-time.Hour), user.ID, false)
	if err != nil || len(totals) != 1 || totals[0].InputTokens != 10 || totals[0].Minutes < 2.9 || totals[0].UserEmail != "a@example.com" {
		t.Fatalf("totals: %+v %v", totals, err)
	}
	if err := store.SetUserDisabled(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateAPIKey(ctx, secret); !errors.Is(err, postgres.ErrInvalidAPIKey) {
		t.Fatalf("disabled user's key works: %v", err)
	}
	if err := store.SetUserDisabled(ctx, user.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateAPIKey(ctx, secret); !errors.Is(err, postgres.ErrInvalidAPIKey) {
		t.Fatal("re-enabling a user restored a revoked key")
	}
	if err := store.RevokeAPIKey(ctx, shared.ID, 0, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateAPIKey(ctx, sharedSecret); !errors.Is(err, postgres.ErrInvalidAPIKey) {
		t.Fatal("revoked key works")
	}
}

func TestSignInBelowMemberRevokesAPIKeys(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	person := overload.User{Issuer: "https://idp", Subject: "2", Email: "b@example.com", Role: overload.RoleMember}
	user, err := store.SignInUser(ctx, person)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := store.CreateAPIKey(ctx, user.ID, "laptop", []string{overload.ScopeModels}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	person.Role = overload.RoleAdmin
	if _, err := store.SignInUser(ctx, person); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateAPIKey(ctx, secret); err != nil {
		t.Fatalf("a promotion revoked the key: %v", err)
	}
	person.Role = overload.RoleViewer
	if _, err := store.SignInUser(ctx, person); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateAPIKey(ctx, secret); !errors.Is(err, postgres.ErrInvalidAPIKey) {
		t.Fatalf("a viewer's key still works: %v", err)
	}
	keys, err := store.ListAPIKeys(ctx, user.ID, false)
	if err != nil || len(keys) != 1 || keys[0].State(time.Now()) != overload.APIKeyRevoked {
		t.Fatalf("keys: %+v %v", keys, err)
	}
}
