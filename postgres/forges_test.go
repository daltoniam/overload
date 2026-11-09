package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/daltoniam/overload"
)

func TestForgeConnections(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("gl-%d", time.Now().UnixNano())
	project := "group/sub/" + name
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE full_name=$1`, project)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM forge_connections WHERE name=$1`, name)
	})
	if _, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeGitLab, BaseURL: "https://gitlab.example.com/", Token: ""}); err == nil {
		t.Fatal("saved a connection without a token")
	}
	saved, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeGitLab, BaseURL: "https://gitlab.example.com/", Token: "glpat-1"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == 0 || saved.BaseURL != "https://gitlab.example.com" || len(saved.WebhookSecret) != 64 {
		t.Fatalf("saved = %+v", saved)
	}
	updated, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeGitLab, BaseURL: "https://gitlab.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadForge(ctx, saved.ID)
	if err != nil || updated.ID != saved.ID || loaded.Token != "glpat-1" || loaded.WebhookSecret != saved.WebhookSecret || loaded.BaseURL != "https://gitlab.example.org" {
		t.Fatalf("update lost the token or secret: %+v %v", loaded, err)
	}
	if _, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeForgejo, BaseURL: "https://gitlab.example.org", Token: "x"}); err == nil {
		t.Fatal("changed a connection's kind")
	}
	listed, err := store.ListForges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, connection := range listed {
		if connection.Token != "" {
			t.Fatal("ListForges returned a token")
		}
	}
	if err := store.SaveRepository(ctx, name, project, true, true); err != nil {
		t.Fatalf("GitLab subgroup project: %v", err)
	}
	if err := store.SaveRepository(ctx, "", project, true, true); err == nil {
		t.Fatal("GitHub accepted a three-part repository name")
	}
	if err := store.SaveRepository(ctx, "missing-host", "acme/api", true, true); err == nil {
		t.Fatal("saved a repository on an unknown host")
	}
	repos, err := store.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, repo := range repos {
		if repo.FullName == project {
			found = repo.ForgeID == saved.ID && repo.Forge == name && repo.Host() == overload.ForgeGitLab
		}
	}
	if !found {
		t.Fatal("repository not listed with its host")
	}
	if err := store.DeleteForge(ctx, saved.ID); err == nil {
		t.Fatal("deleted a git host that has repositories")
	}
	if _, err := store.Pool.Exec(ctx, `DELETE FROM repositories WHERE forge_id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteForge(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadForge(ctx, saved.ID); err != ErrUnknownForge {
		t.Fatalf("deleted connection still loads: %v", err)
	}
}
