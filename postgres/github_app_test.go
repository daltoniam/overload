package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/daltoniam/overload/github"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestApplyInstallationEvent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := time.Now().UnixNano()
	installationID := id % 1_000_000_000_000
	repoA := fmt.Sprintf("test-inst-%d/a", id)
	repoB := fmt.Sprintf("test-inst-%d/b", id)
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE github_id BETWEEN $1 AND $2 OR full_name IN ($3,$4)`, installationID+1, installationID+3, repoA, repoB)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM github_installations WHERE id=$1`, installationID)
	})
	if _, err := store.Pool.Exec(ctx, `INSERT INTO repositories (full_name, enabled) VALUES ($1, true)`, repoA); err != nil {
		t.Fatal(err)
	}
	event := func(action string, repos ...github.InstallationRepository) github.InstallationEvent {
		var e github.InstallationEvent
		e.Action = action
		e.Installation.ID = installationID
		e.Installation.Account.Login = "acme"
		e.Installation.Account.Type = "Organization"
		e.Repositories = repos
		return e
	}
	a := github.InstallationRepository{ID: installationID + 1, FullName: repoA}
	b := github.InstallationRepository{ID: installationID + 2, FullName: repoB}
	delivery := fmt.Sprintf("inst-%d", id)
	if ok, err := store.ApplyInstallationEvent(ctx, delivery, "installation", []byte(`{}`), event("created", a, b)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := store.ApplyInstallationEvent(ctx, delivery, "installation", []byte(`{}`), event("created", a, b)); err != nil || ok {
		t.Fatal("duplicate delivery applied", ok, err)
	}
	for _, repo := range []string{repoA, repoB} {
		if got := repoInstallation(t, store, repo); got != installationID {
			t.Fatalf("%s installation=%d", repo, got)
		}
	}
	var enabledA, enabledB bool
	_ = store.Pool.QueryRow(ctx, `SELECT enabled FROM repositories WHERE full_name=$1`, repoA).Scan(&enabledA)
	_ = store.Pool.QueryRow(ctx, `SELECT enabled FROM repositories WHERE full_name=$1`, repoB).Scan(&enabledB)
	if !enabledA || enabledB {
		t.Fatalf("existing repo should keep enabled, new repo starts disabled: a=%v b=%v", enabledA, enabledB)
	}

	removed := event("removed")
	removed.RepositoriesRemoved = []github.InstallationRepository{a}
	if _, err := store.ApplyInstallationEvent(ctx, delivery+"-rm", "installation_repositories", []byte(`{}`), removed); err != nil {
		t.Fatal(err)
	}
	_ = store.Pool.QueryRow(ctx, `SELECT enabled FROM repositories WHERE full_name=$1`, repoA).Scan(&enabledA)
	if got := repoInstallation(t, store, repoA); got != 0 || enabledA {
		t.Fatalf("removed repo still installed=%d enabled=%v", got, enabledA)
	}

	if _, err := store.ApplyInstallationEvent(ctx, delivery+"-suspend", "installation", []byte(`{}`), event("suspend")); err != nil {
		t.Fatal(err)
	}
	installations, err := store.ListGitHubInstallations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, installation := range installations {
		if installation.ID == installationID {
			found = installation.Suspended && installation.Repositories == 1
		}
	}
	if !found {
		t.Fatalf("suspended installation not listed: %+v", installations)
	}

	replacement := github.InstallationRepository{ID: installationID + 3, FullName: repoA}
	added := event("added")
	added.RepositoriesAdded = []github.InstallationRepository{replacement, {ID: b.ID, FullName: repoB + "-renamed"}}
	if _, err := store.ApplyInstallationEvent(ctx, delivery+"-reuse", "installation_repositories", []byte(`{}`), added); err != nil {
		t.Fatalf("name reused by a different repository: %v", err)
	}
	var reusedID, renamedID int64
	_ = store.Pool.QueryRow(ctx, `SELECT github_id FROM repositories WHERE full_name=$1`, repoA).Scan(&reusedID)
	_ = store.Pool.QueryRow(ctx, `SELECT github_id FROM repositories WHERE full_name=$1`, repoB+"-renamed").Scan(&renamedID)
	if reusedID != replacement.ID || renamedID != b.ID {
		t.Fatalf("name now maps to %d (want %d); renamed repo maps to %d (want %d)", reusedID, replacement.ID, renamedID, b.ID)
	}
	var stale int
	_ = store.Pool.QueryRow(ctx, `SELECT count(*) FROM repositories WHERE github_id=$1 AND full_name LIKE '% (replaced %' AND NOT enabled`, a.ID).Scan(&stale)
	if stale != 1 {
		t.Fatal("old repository kept the reused name")
	}
	repoB += "-renamed"

	if _, err := store.ApplyInstallationEvent(ctx, delivery+"-delete", "installation", []byte(`{}`), event("deleted")); err != nil {
		t.Fatal(err)
	}
	if got := repoInstallation(t, store, repoB); got != 0 {
		t.Fatal("repository still linked after uninstall")
	}
}

func TestGitHubAppCredentials(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.LoadGitHubApp(ctx); !errors.Is(err, ErrNoGitHubApp) {
		t.Skip("database already has GitHub App credentials; not overwriting them")
	}
	t.Cleanup(func() { _, _ = store.Pool.Exec(ctx, `DELETE FROM github_app`) })
	if secret, err := store.GitHubWebhookSecret(ctx); err != nil || secret != "" {
		t.Fatal(secret, err)
	}
	if err := store.SaveGitHubApp(ctx, github.AppCredentials{AppID: 1}); err == nil {
		t.Fatal("saved incomplete credentials")
	}
	if err := store.SaveGitHubApp(ctx, github.AppCredentials{AppID: 9, Slug: "x", PrivateKey: "pem", WebhookSecret: "hook"}); err != nil {
		t.Fatal(err)
	}
	if secret, err := store.GitHubWebhookSecret(ctx); err != nil || secret != "hook" {
		t.Fatal(secret, err)
	}
}

func TestSkipReason(t *testing.T) {
	linked := repoFacts{found: true, enabled: true, installation: 7}
	manual := repoFacts{found: true, enabled: true}
	for _, test := range []struct {
		name     string
		delivery PullRequestDelivery
		repo     repoFacts
		want     string
	}{
		{"app delivery for linked repo", PullRequestDelivery{Eligible: true, InstallationID: 7}, linked, ""},
		{"plain webhook for manual repo", PullRequestDelivery{Eligible: true}, manual, ""},
		{"draft or bot PR", PullRequestDelivery{InstallationID: 7}, linked, skipNotEligible},
		{"unknown repository", PullRequestDelivery{Eligible: true}, repoFacts{}, skipNotEnabled},
		{"disabled repository", PullRequestDelivery{Eligible: true, InstallationID: 7}, repoFacts{found: true, installation: 7}, skipNotEnabled},
		{"other installation", PullRequestDelivery{Eligible: true, InstallationID: 8}, linked, skipInstallMismatch},
		{"linked repo, delivery without installation", PullRequestDelivery{Eligible: true}, linked, skipInstallMismatch},
		{"app delivery for manual repo", PullRequestDelivery{Eligible: true, InstallationID: 7}, manual, skipNotLinked},
		{"suspended installation", PullRequestDelivery{Eligible: true, InstallationID: 7}, repoFacts{found: true, enabled: true, installation: 7, suspended: true}, skipSuspended},
	} {
		if got := skipReason(test.delivery, test.repo); got != test.want {
			t.Errorf("%s: got %q, want %q", test.name, got, test.want)
		}
	}
}

func TestIngestPRMatchesRepositoryIdentity(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	id := time.Now().UnixNano()
	installationID := id % 1_000_000_000_000
	githubID := installationID + 1
	repo := fmt.Sprintf("test-ingest-inst-%d/a", id)
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE repository_full_name IN ($1, $2)`, repo, repo+"-renamed")
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE github_id=$1 OR full_name=$2`, githubID, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM github_installations WHERE id=$1`, installationID)
	})
	if _, err := store.Pool.Exec(ctx, `INSERT INTO github_installations (id, account_login, account_type) VALUES ($1,'acme','Organization')`, installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO repositories (full_name, github_id, enabled, installation_id) VALUES ($1, $2, true, $3)`, repo, githubID, installationID); err != nil {
		t.Fatal(err)
	}
	n := 0
	reason := func(delivery PullRequestDelivery) string {
		t.Helper()
		n++
		delivery.DeliveryID = fmt.Sprintf("identity-%d-%d", id, n)
		delivery.Action, delivery.Payload, delivery.PR, delivery.HeadSHA, delivery.BaseSHA, delivery.Eligible = "opened", []byte(`{}`), 1, "head", "base", true
		if _, err := store.IngestPR(ctx, client, delivery); err != nil {
			t.Fatal(err)
		}
		var skip string
		if err := store.Pool.QueryRow(ctx, `SELECT skip_reason FROM webhook_deliveries WHERE delivery_id=$1`, delivery.DeliveryID).Scan(&skip); err != nil {
			t.Fatal(err)
		}
		return skip
	}
	if got := reason(PullRequestDelivery{RepoName: repo, RepoID: githubID, InstallationID: installationID + 5}); got != skipInstallMismatch {
		t.Fatalf("mismatched installation: %q", got)
	}
	if got := reason(PullRequestDelivery{RepoName: repo + "-renamed", RepoID: githubID, InstallationID: installationID}); got != skipNoBinding {
		t.Fatalf("renamed repository not matched by GitHub ID: %q", got)
	}
	if got := reason(PullRequestDelivery{RepoName: repo, RepoID: githubID + 99, InstallationID: installationID}); got != skipNotEnabled {
		t.Fatalf("different repository with the same name matched: %q", got)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE github_installations SET suspended_at=now() WHERE id=$1`, installationID); err != nil {
		t.Fatal(err)
	}
	if got := reason(PullRequestDelivery{RepoName: repo, RepoID: githubID, InstallationID: installationID}); got != skipSuspended {
		t.Fatalf("suspended installation: %q", got)
	}
}

func repoInstallation(t *testing.T, store *Store, name string) int64 {
	t.Helper()
	var id int64
	if err := store.Pool.QueryRow(context.Background(), `SELECT COALESCE(installation_id, 0) FROM repositories WHERE full_name = $1`, name).Scan(&id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return id
}
