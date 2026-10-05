package postgres

import (
	"context"
	"fmt"
	"testing"
)

func TestUISearchBeyondFirstPage(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	prefix := "ui-search-test"
	t.Cleanup(func() { _, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE trigger LIKE $1`, prefix+"%") })
	for index := 0; index < 105; index++ {
		trigger := fmt.Sprintf("%s-%03d", prefix, index)
		if _, err := store.Pool.Exec(ctx, `INSERT INTO runs(trigger,kind,status,mode,dry_run,head_sha,base_sha) VALUES ($1,'pr_review','completed','inline',true,'head','base')`, trigger); err != nil {
			t.Fatal(err)
		}
	}
	runs, total, err := store.SearchRuns(ctx, ListFilter{Query: prefix + "-000", Page: 1})
	if err != nil || total != 1 || len(runs) != 1 {
		t.Fatalf("older record search: total=%d rows=%d err=%v", total, len(runs), err)
	}
	_, total, err = store.SearchRuns(ctx, ListFilter{Query: prefix, Page: 2})
	if err != nil || total != 105 {
		t.Fatalf("count: %d %v", total, err)
	}
	_, total, err = store.SearchRuns(ctx, ListFilter{Query: prefix + "%", Page: 1})
	if err != nil || total != 0 {
		t.Fatalf("wildcard not literal: %d %v", total, err)
	}
}

func TestUILiteralPattern(t *testing.T) {
	if got := literalPattern(`a%_\b`); got != `%a\%\_\\b%` {
		t.Fatalf("pattern: %q", got)
	}
}
