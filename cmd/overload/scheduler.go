package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/queue"
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

// catchUpWebhooks redelivers GitHub webhooks that never reached overload,
// every few minutes, so a restart, tunnel outage or DNS failure does not
// lose reviews. It needs the GitHub App and does nothing without one, or
// when OVERLOAD_WEBHOOK_CATCHUP=0.
func catchUpWebhooks(ctx context.Context, store *postgres.Store) {
	if os.Getenv("OVERLOAD_WEBHOOK_CATCHUP") == "0" {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		client, err := queue.AppClient(ctx, store)
		if err != nil {
			continue
		}
		scan, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if _, err := queue.CatchUpWebhooks(scan, client, store, time.Now()); err != nil && ctx.Err() == nil {
			slog.Warn("webhook catch-up failed", "error", err)
		}
		cancel()
	}
}
