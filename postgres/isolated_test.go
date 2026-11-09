package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
)

// isolatedStore returns a store whose tables live in a fresh schema, for
// tests that change installation-wide state (such as the model policy)
// which tests in other packages, running at the same time, would see.
func isolatedStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	admin, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	schema := "test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Pool.Close()
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Pool.Close()
		_, _ = admin.Pool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Pool.Close()
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}
