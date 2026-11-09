package main

import (
	"bytes"
	"context"
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
	now := time.Now()
	store.keys[id-1].RevokedAt = &now
	return nil
}

func TestRunKeys(t *testing.T) {
	store := &fakeKeys{}
	var out bytes.Buffer
	ctx := context.Background()
	if err := runKeys(ctx, store, []string{"create", "--name", "ci", "--scopes", "models,reviews", "--days", "30"}, &out); err != nil || !strings.Contains(out.String(), "ovl_abc_secret") || store.keys[0].UserID != 0 {
		t.Fatalf("create: %v %s", err, out.String())
	}
	if err := runKeys(ctx, store, []string{"create", "--name", "x", "--days", "500"}, &out); err == nil {
		t.Fatal("created a key that lasts 500 days")
	}
	if err := runKeys(ctx, store, []string{"revoke", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runKeys(ctx, store, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "revoked") || !strings.Contains(out.String(), "shared") {
		t.Fatalf("list: %v %s", err, out.String())
	}
	if err := runKeys(ctx, store, []string{"revoke", "x"}, &out); err == nil {
		t.Fatal("accepted a bad ID")
	}
}
