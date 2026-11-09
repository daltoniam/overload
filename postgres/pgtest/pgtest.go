// Package pgtest gives integration tests a Postgres schema of their own.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/daltoniam/overload/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Isolated returns a migrated store whose tables live in a fresh schema,
// dropped when the test ends, for tests that change installation-wide state
// (such as the model policy) that tests in other packages, which share the
// database and run at the same time, would otherwise see. It skips the test
// without DATABASE_URL.
func Isolated(t testing.TB) *postgres.Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	admin, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Pool.Close)
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	schema := "test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Pool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := &postgres.Store{Pool: pool}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}
