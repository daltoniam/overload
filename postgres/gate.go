package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daltoniam/overload"
)

// Run triggers. A person asks for reviews in the UI or the CLI (manual);
// API keys and workload tokens use the review API (api).
const (
	TriggerWebhook = "webhook"
	TriggerManual  = "manual"
	TriggerAPI     = "api"
)

// recordedGate is the merge gate verdict stored on a run.
type recordedGate struct {
	State      string   `json:"state"`
	HumanFiles []string `json:"human_files"`
}

// RecordGate stores the merge gate verdict a run's commit status reported.
func (s *Store) RecordGate(ctx context.Context, runID int64, gate overload.Gate) error {
	data, err := json.Marshal(recordedGate{State: gate.State, HumanFiles: append([]string{}, gate.HumanFiles...)})
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `UPDATE runs SET gate = $2 WHERE id = $1`, runID, data)
	return err
}

// GateHistory is what earlier reviews of a pull request mean for a run's
// merge gate.
type GateHistory struct {
	// HeldFailure is set when the latest earlier verdict on the same commit
	// failed and this run was not started by a person, so it cannot clear
	// that verdict.
	HeldFailure bool
	// PostedHumanFiles are the human review path files of overload's last
	// posted review of the pull request; HumanFilesKnown is false when
	// there is none.
	PostedHumanFiles []string
	HumanFilesKnown  bool
}

// gateHistoryColumns selects a run r's GateHistory: whether it is held by
// an earlier failing verdict, and the last posted review's human files.
const gateHistoryColumns = `r.trigger <> 'manual' AND COALESCE((SELECT e.gate->>'state' FROM runs e WHERE e.repository_id = r.repository_id AND e.pr_number = r.pr_number AND e.head_sha = r.head_sha AND e.id < r.id AND NOT e.dry_run AND e.gate <> '{}' ORDER BY e.id DESC LIMIT 1) = 'failure', false),
COALESCE((SELECT e.gate->'human_files' FROM runs e WHERE e.repository_id = r.repository_id AND e.pr_number = r.pr_number AND e.id < r.id AND e.post_status IN ('posted', 'nothing_new') AND e.gate <> '{}' ORDER BY e.id DESC LIMIT 1), 'null')`

// LoadGateHistory returns the GateHistory of a run.
func (s *Store) LoadGateHistory(ctx context.Context, runID int64) (GateHistory, error) {
	var history GateHistory
	var human []byte
	if err := s.Pool.QueryRow(ctx, `SELECT `+gateHistoryColumns+` FROM runs r WHERE r.id = $1`, runID).Scan(&history.HeldFailure, &human); err != nil {
		return history, err
	}
	return history, history.readHumanFiles(human)
}

func (history *GateHistory) readHumanFiles(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	history.HumanFilesKnown = true
	if err := json.Unmarshal(data, &history.PostedHumanFiles); err != nil {
		return fmt.Errorf("read earlier human review files: %w", err)
	}
	return nil
}
