package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/daltoniam/overload/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

func scanSchedules(ctx context.Context, store *postgres.Store, client *river.Client[pgx.Tx]) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if _, err := store.EnqueueDueSchedules(ctx, client); err != nil && ctx.Err() == nil {
			slog.Error("schedule scan failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
