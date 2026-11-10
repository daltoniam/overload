package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
)

type fakeKeys struct{ keys []overload.APIKey }

func (store *fakeKeys) CreateAPIKey(_ context.Context, userID int64, name string, scopes []string, lifetime time.Duration) (overload.APIKey, string, error) {
	if err := overload.ValidateAPIKey(name, scopes, lifetime); err != nil {
		return overload.APIKey{}, "", err
	}
	key := overload.APIKey{ID: int64(len(store.keys) + 1), UserID: userID, Name: name, Prefix: "ovl_abc", Scopes: scopes, ExpiresAt: time.Now().Add(lifetime)}
	store.keys = append(store.keys, key)
	return key, "ovl_abc_secret", nil
}
func (store *fakeKeys) ListAPIKeys(context.Context, int64, bool) ([]overload.APIKey, error) {
	return store.keys, nil
}
func (store *fakeKeys) RevokeAPIKey(_ context.Context, id, _ int64, _ bool) error {
	if id < 1 || int(id) > len(store.keys) || store.keys[id-1].RevokedAt != nil {
		return overload.ErrInvalidAPIKey
	}
	now := time.Now()
	store.keys[id-1].RevokedAt = &now
	return nil
}

func TestRunKeys(t *testing.T) {
	store := &fakeKeys{}
	var out bytes.Buffer
	ctx := context.Background()
	if err := runKeys(ctx, store, []string{"create", "--name", "ci", "--days", "30"}, &out); err != nil || !strings.Contains(out.String(), "ovl_abc_secret") || store.keys[0].UserID != 0 || !slices.Equal(store.keys[0].Scopes, []string{overload.ScopeModels}) {
		t.Fatalf("create: %v %s", err, out.String())
	}
	if err := runKeys(ctx, store, []string{"create", "--name", "ci", "--scopes", "models,reviews"}, &out); err != nil || !slices.Equal(store.keys[len(store.keys)-1].Scopes, []string{overload.ScopeModels, overload.ScopeReviews}) {
		t.Fatalf("create with both scopes: %v", err)
	}
	if err := runKeys(ctx, store, []string{"create", "--name", "ci", "--scopes", " models, reviews,,models "}, &out); err != nil || !slices.Equal(store.keys[len(store.keys)-1].Scopes, []string{overload.ScopeModels, overload.ScopeReviews}) {
		t.Fatalf("create with spaced, repeated scopes: %v %v", err, store.keys[len(store.keys)-1].Scopes)
	}
	if err := runKeys(ctx, store, []string{"create", "--name", "ci", "--scopes", "admin"}, &out); err == nil {
		t.Fatal("created a key with an unknown scope")
	}
	if err := runKeys(ctx, store, []string{"create", "--name", "x", "--days", "500"}, &out); err == nil {
		t.Fatal("created a key that lasts 500 days")
	}
	if err := runKeys(ctx, store, []string{"revoke", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := runKeys(ctx, store, []string{"revoke", "1"}, &out); err == nil || !strings.Contains(err.Error(), "no active API key 1") {
		t.Fatalf("revoking a revoked key: %v", err)
	}
	store.keys = append(store.keys, overload.APIKey{ID: 2, Name: "old", Prefix: "ovl_old", Scopes: []string{overload.ScopeModels}, ExpiresAt: time.Now().Add(-time.Hour)})
	out.Reset()
	if err := runKeys(ctx, store, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "revoked") || !strings.Contains(out.String(), "expired") || !strings.Contains(out.String(), "shared") || strings.Contains(out.String(), "newest") {
		t.Fatalf("list: %v %s", err, out.String())
	}
	for len(store.keys) < overload.APIKeyListLimit {
		store.keys = append(store.keys, overload.APIKey{ID: int64(len(store.keys) + 1), ExpiresAt: time.Now().Add(time.Hour)})
	}
	out.Reset()
	if err := runKeys(ctx, store, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "Showing the 500 newest keys.") {
		t.Fatalf("capped list: %v", err)
	}
	if err := runKeys(ctx, store, []string{"revoke", "x"}, &out); err == nil {
		t.Fatal("accepted a bad ID")
	}
}
