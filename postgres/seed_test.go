package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestIsLocalDatabase(t *testing.T) {
	for _, test := range []struct {
		dsn     string
		allowed bool
	}{
		{"postgres://user:pass@localhost:5432/test?sslmode=disable", true},
		{"postgres://user:pass@127.0.0.1:5432/test?sslmode=disable", true},
		{"postgres://user:pass@[::1]:5432/test?sslmode=disable", true},
		{"postgres://user:pass@database.example:5432/test", false},
		{"postgres://user:pass@192.168.1.3:5432/test", false},
		{"", false},
	} {
		if err := IsLocalDatabase(test.dsn); (err == nil) != test.allowed {
			t.Errorf("local database allowed=%v for %q, err=%v", test.allowed, test.dsn, err)
		}
	}
}

func TestSeedLocalDemo(t *testing.T) {
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
	var present int
	if err := store.Pool.QueryRow(ctx, `SELECT count(*) FROM model_profiles WHERE name='demo-local-model'`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 0 {
		t.Skip("demo seed already exists; preserve user data")
	}
	created, err := store.SeedLocalDemo(ctx)
	if err != nil || !created {
		t.Fatalf("seed: created=%v err=%v", created, err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM job_outputs WHERE run_id IN (SELECT id FROM runs WHERE trigger='demo_seed' AND schedule_id=(SELECT id FROM schedules WHERE name='demo-daily-summary'))`)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE trigger='demo_seed' AND schedule_id=(SELECT id FROM schedules WHERE name='demo-daily-summary')`)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM schedules WHERE name='demo-daily-summary'`)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM trigger_bindings WHERE repository_full_name IN ('demo/alpha','demo/beta')`)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE full_name IN ('demo/alpha','demo/beta')`)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name IN ('demo-pr-workflow','demo-scheduled-workflow')`)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name IN ('demo-pr-reviewer','demo-summary-agent')`)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name IN ('demo-review-entry','demo-summary-entry'))`)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name IN ('demo-review-entry','demo-summary-entry')`)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name='demo-local-model'`)
	})
	created, err = store.SeedLocalDemo(ctx)
	if err != nil || created {
		t.Fatalf("repeat seed: created=%v err=%v", created, err)
	}
	repos, err := store.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, repo := range repos {
		if strings.HasPrefix(repo.FullName, "demo/") {
			seen++
			if repo.Enabled || !repo.DryRun {
				t.Fatalf("unsafe demo repository: %+v", repo)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("got %d demo repositories", seen)
	}
	var runID int64
	if err := store.Pool.QueryRow(ctx, `SELECT id FROM runs WHERE trigger='demo_seed' AND schedule_id=(SELECT id FROM schedules WHERE name='demo-daily-summary')`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	output, err := store.ReadJobOutput(ctx, runID)
	if err != nil || len(output.Agents) != 1 || !strings.Contains(output.Agents[0].Text, "No model was called") {
		t.Fatalf("demo output: %+v err=%v", output, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE schedules SET enabled=true WHERE name='demo-daily-summary'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SeedLocalDemo(ctx); err == nil {
		t.Fatal("seed accepted changed schedule")
	}
}
