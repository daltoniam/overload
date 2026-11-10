package queue

import (
	"context"
	"errors"
	"time"

	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
)

// stoppedByShutdown reports whether a job's context was cancelled rather
// than timed out: River cancels running jobs when a stopping server's soft
// stop runs out, while a job timeout ends the context with a deadline.
func stoppedByShutdown(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// cancelIfInterrupted cancels, instead of retrying, the job of a run that
// shutdown stopped while it was running. The run stays running and the next
// start queues it again with a new job, so a retry of this one would review
// it twice. A run that had not started yet keeps the error and is retried.
func cancelIfInterrupted(ctx context.Context, store *postgres.Store, runID int64, err error) error {
	if err == nil || !stoppedByShutdown(ctx) {
		return err
	}
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var status string
	if lookupErr := store.Pool.QueryRow(lookupCtx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status); lookupErr != nil || status != "running" {
		return err
	}
	return river.JobCancel(err)
}
