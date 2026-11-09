package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEvalRepositoryIsExact(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	repo := fmt.Sprintf("test-eval-repo-%d/api", time.Now().UnixNano())
	var repoID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name) VALUES ($1) RETURNING id`, repo).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE id=$1`, repoID) })
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if id, _, err := evalRepository(ctx, tx, EvalRequest{Repository: repo}); err != nil || id != repoID {
		t.Fatalf("by name = %d %v", id, err)
	}
	if id, _, err := evalRepository(ctx, tx, EvalRequest{Repository: repo, RepositoryID: repoID}); err != nil || id != repoID {
		t.Fatalf("by ID = %d %v", id, err)
	}
	// An ID of another repository never falls back to the name.
	if _, _, err := evalRepository(ctx, tx, EvalRequest{Repository: repo, RepositoryID: repoID + 1_000_000}); err == nil || !strings.Contains(err.Error(), "not a repository overload knows") {
		t.Fatalf("mismatched ID = %v", err)
	}
	if _, _, err := evalRepository(ctx, tx, EvalRequest{Repository: "nobody/" + repo}); err == nil {
		t.Fatal("unknown repository accepted")
	}
}
