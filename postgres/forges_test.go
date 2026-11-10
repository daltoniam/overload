package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
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
	if _, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeGitLab, BaseURL: "https://attacker.example"}); !errors.Is(err, overload.ErrTokenRequired) {
		t.Fatalf("stored token reused for another host: %v", err)
	}
	updated, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeGitLab, BaseURL: "https://gitlab.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeGitLab, BaseURL: "https://gitlab.example.org", Token: "glpat-2"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadForge(ctx, saved.ID)
	if err != nil || updated.ID != saved.ID || moved.ID != saved.ID || loaded.Token != "glpat-2" || loaded.WebhookSecret != saved.WebhookSecret || loaded.BaseURL != "https://gitlab.example.org" {
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

func TestForgeRenameMatchesBindingsByTheNewName(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	name := fmt.Sprintf("rename-%d", time.Now().UnixNano())
	oldPath, newPath := "acme/"+name, "acme/"+name+"-renamed"
	t.Cleanup(func() {
		for _, path := range []string{oldPath, newPath} {
			_, _ = store.Pool.Exec(ctx, `DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id IN (SELECT id FROM repositories WHERE full_name=$1))`, path)
			_, _ = store.Pool.Exec(ctx, `UPDATE webhook_deliveries SET run_id=NULL WHERE repository_full_name=$1`, path)
			_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id IN (SELECT id FROM repositories WHERE full_name=$1)`, path)
			_, _ = store.Pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE repository_full_name=$1`, path)
			_, _ = store.Pool.Exec(ctx, `DELETE FROM trigger_bindings WHERE repository_full_name=$1`, path)
			_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE full_name=$1`, path)
		}
		_, _ = store.Pool.Exec(ctx, `DELETE FROM forge_connections WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	connection, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeForgejo, BaseURL: "https://codeberg.example", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: "http://127.0.0.1:1/v1", Model: "m", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Model: name, Prompt: "Review.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "pr_review", Agents: []string{name}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO repositories (full_name, enabled, dry_run, forge_id, forge_repo_id) VALUES ($1, true, true, $2, 4242)`, oldPath, connection.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBinding(ctx, overload.TriggerBinding{Source: overload.ForgeForgejo, Event: "pull_request", Action: "opened", Repository: newPath, Workflow: name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `DELETE FROM trigger_bindings WHERE source='forgejo' AND repository_full_name=''`); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	delivery := PullRequestDelivery{DeliveryID: name, Action: "opened", RepoName: newPath, RepoID: 4242, ForgeID: connection.ID, Forge: overload.ForgeForgejo, PR: 1, HeadSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("a", 40), Eligible: true, Payload: []byte(`{"n":1}`)}
	if _, err := store.IngestPR(ctx, client, delivery); err != nil {
		t.Fatal(err)
	}
	outcome, reason, _, err := store.DeliveryOutcome(ctx, overload.ForgeForgejo, name)
	if err != nil || outcome != "queued" {
		t.Fatalf("delivery after rename: %s %s %v", outcome, reason, err)
	}
}
