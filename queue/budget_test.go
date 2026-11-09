package queue

import (
	"context"
	"strings"
	"testing"

	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
)

func TestSkipStatusWorkerSetsTheChosenStatus(t *testing.T) {
	status := &fakeStatus{}
	worker := &SkipStatusWorker{Status: status}
	sha := strings.Repeat("c", 40)
	job := &river.Job[postgres.SkipStatusArgs]{Args: postgres.SkipStatusArgs{Repository: "acme/api", InstallationID: 7, SHA: sha, State: "success", Description: "Overload skipped: monthly review budget reached"}}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if len(status.states) != 1 || status.states[0] != "success" || status.shas[0] != sha {
		t.Fatalf("statuses = %+v", status)
	}
	job.Args.InstallationID = 0
	if err := worker.Work(context.Background(), job); err != nil || len(status.states) != 1 {
		t.Fatalf("status set without an installation: %v %+v", err, status)
	}
}
