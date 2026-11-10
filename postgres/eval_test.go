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
	if found, err := evalRepository(ctx, tx, EvalRequest{Repository: repo}); err != nil || found.id != repoID || found.forge != nil {
		t.Fatalf("by name = %+v %v", found, err)
	}
	if found, err := evalRepository(ctx, tx, EvalRequest{Repository: repo, RepositoryID: repoID}); err != nil || found.id != repoID {
		t.Fatalf("by ID = %+v %v", found, err)
	}
	// An ID of another repository never falls back to the name.
	if _, err := evalRepository(ctx, tx, EvalRequest{Repository: repo, RepositoryID: repoID + 1_000_000}); err == nil || !strings.Contains(err.Error(), "not a repository overload knows") {
		t.Fatalf("mismatched ID = %v", err)
	}
	if _, err := evalRepository(ctx, tx, EvalRequest{Repository: "nobody/" + repo}); err == nil {
		t.Fatal("unknown repository accepted")
	}
	if id, err := store.RepositoryHostID(ctx, repoID); err != nil || id != 0 {
		t.Fatalf("GitHub ID before the App reported it = %d %v", id, err)
	}
	githubID := time.Now().UnixNano() % 1_000_000_000_000
	if _, err := store.Pool.Exec(ctx, `UPDATE repositories SET github_id=$2 WHERE id=$1`, repoID, githubID); err != nil {
		t.Fatal(err)
	}
	if id, err := store.RepositoryHostID(ctx, repoID); err != nil || id != githubID {
		t.Fatalf("GitHub ID = %d %v", id, err)
	}
	// The same path on a GitLab connection: the name alone is ambiguous,
	// the ID names it and carries its connection and GitLab project ID.
	var forgeID, forgeRepoID int64
	if err := store.Pool.QueryRow(ctx, `INSERT INTO forge_connections (name, kind, base_url, token, webhook_secret) VALUES ($1, 'gitlab', 'https://gitlab.example.com', 't', 's') RETURNING id`, fmt.Sprintf("eval-%d", githubID)).Scan(&forgeID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow(ctx, `INSERT INTO repositories (full_name, forge_id, forge_repo_id) VALUES ($1, $2, 77) RETURNING id`, repo, forgeID).Scan(&forgeRepoID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE id=$1`, forgeRepoID)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM forge_connections WHERE id=$1`, forgeID)
	})
	tx, err = store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := evalRepository(ctx, tx, EvalRequest{Repository: repo}); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("ambiguous name = %v", err)
	}
	if found, err := evalRepository(ctx, tx, EvalRequest{Repository: repo, RepositoryID: forgeRepoID}); err != nil || found.id != forgeRepoID || found.forge == nil || *found.forge != forgeID {
		t.Fatalf("GitLab repository by ID = %+v %v", found, err)
	}
	if id, err := store.RepositoryHostID(ctx, forgeRepoID); err != nil || id != 77 {
		t.Fatalf("GitLab project ID = %d %v", id, err)
	}
}
