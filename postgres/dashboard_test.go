package postgres

import (
	"context"
	"testing"
	"time"
)

func TestDashboardStatistics(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2040, 1, 14, 12, 0, 0, 0, time.UTC)
	prefix := "dashboard-stat-test"
	t.Cleanup(func() { _, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE trigger=$1`, prefix) })
	insert := func(status, kind string, at time.Time) {
		t.Helper()
		_, err := store.Pool.Exec(ctx, `INSERT INTO runs(trigger,kind,status,mode,dry_run,head_sha,base_sha,created_at) VALUES ($1,$2,$3,'inline',true,'head','base',$4)`, prefix, kind, status, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 105; index++ {
		insert("completed", "pr_review", now.Add(-time.Hour))
	}
	insert("failed", "scheduled_prompt", time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC))
	insert("running", "pr_review", now)
	insert("superseded", "pr_review", now.Add(-time.Hour))
	insert("failed", "pr_review", time.Date(2039, 12, 31, 23, 59, 59, 0, time.UTC))
	insert("failed", "pr_review", now.Add(time.Second))
	stats, err := store.DashboardStatistics(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 108 || stats.Completed != 105 || stats.Failed != 1 || stats.Active != 1 || len(stats.Days) != 14 {
		t.Fatalf("wrong aggregate: %+v", stats)
	}
	if stats.Days[0].Count != 1 || stats.Days[13].Count != 107 || stats.Kinds[1].Count != 1 {
		t.Fatalf("wrong buckets: %+v", stats)
	}
	dayTotal, statusTotal, kindTotal := 0, 0, 0
	for _, value := range stats.Days {
		dayTotal += value.Count
	}
	for _, value := range stats.Outcomes {
		statusTotal += value.Count
	}
	for _, value := range stats.Kinds {
		kindTotal += value.Count
	}
	if dayTotal != stats.Total || statusTotal != stats.Total || kindTotal != stats.Total {
		t.Fatal("charts disagree with total")
	}
	empty, err := store.DashboardStatistics(ctx, now.AddDate(1, 0, 0))
	if err != nil || empty.Total != 0 || len(empty.Days) != 14 {
		t.Fatalf("empty window: %+v %v", empty, err)
	}
}
