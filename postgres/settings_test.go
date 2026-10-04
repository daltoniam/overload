package postgres

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

func TestReviewSettingsStore(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	first := overload.ReviewSettings{Name: "test-settings-first", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "bonsai-2-27b", PromptProfile: "context", IsDefault: true}
	second := overload.ReviewSettings{Name: "test-settings-second", Provider: "openaicompat", ConnectionKind: "hosted", BaseURL: "http://127.0.0.1:8081/v1", Model: "other", PromptProfile: "switchboard-go", APIKeyEnv: "TEST_MODEL_TOKEN", Concurrency: 8, ReasoningParam: "reasoning_effort", ReasoningEffort: "high", MaxOutputTokens: 32768}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM model_profiles WHERE name IN ($1, $2)`, first.Name, second.Name)
		_, _ = store.Pool.Exec(context.Background(), `UPDATE model_profiles SET is_default=true WHERE id=(SELECT id FROM model_profiles ORDER BY id LIMIT 1) AND NOT EXISTS(SELECT 1 FROM model_profiles WHERE is_default=true)`)
		store.Pool.Close()
	})
	if _, err := store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name IN ($1, $2)`, first.Name, second.Name); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReviewSettings(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReviewSettings(ctx, second); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetReviewSettings(ctx, first.Name)
	if err != nil || !loaded.IsDefault || loaded.Model != first.Model || loaded.Concurrency != 1 {
		t.Fatalf("first profile: %+v %v", loaded, err)
	}
	if err := store.DeleteReviewSettings(ctx, first.Name); err == nil {
		t.Fatal("deleted default profile")
	}
	second.IsDefault = true
	if err := store.SaveReviewSettings(ctx, second); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.GetReviewSettings(ctx, "")
	if err != nil || loaded.Name != second.Name || loaded.ConnectionKind != "hosted" || loaded.PromptProfile != second.PromptProfile || loaded.APIKeyEnv != second.APIKeyEnv || loaded.Concurrency != 8 || loaded.ReasoningParam != "reasoning_effort" || loaded.ReasoningEffort != "high" || loaded.MaxOutputTokens != 32768 {
		t.Fatalf("default profile: %+v %v", loaded, err)
	}
	settings, err := store.ListReviewSettings(ctx)
	if err != nil || len(settings) < 2 {
		t.Fatalf("profiles: %+v %v", settings, err)
	}
	if err := store.DeleteReviewSettings(ctx, first.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetReviewSettings(ctx, first.Name); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted profile still present: %v", err)
	}
}
