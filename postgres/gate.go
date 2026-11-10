package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5/pgconn"
)

// Run triggers. A person asks for reviews in the UI or the CLI (manual);
// API keys and workload tokens use the review API (api).
const (
	TriggerWebhook = "webhook"
	TriggerManual  = "manual"
	TriggerAPI     = "api"
)

// recordedGate is the merge gate verdict stored on a run. Held is set when
// an earlier failing verdict kept the run from passing.
type recordedGate struct {
	State      string   `json:"state"`
	HumanFiles []string `json:"human_files"`
	Held       bool     `json:"held,omitempty"`
}

type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// RecordGate stores the merge gate verdict a run's commit status reported.
func (s *Store) RecordGate(ctx context.Context, runID int64, gate overload.Gate) error {
	return RecordGate(ctx, s.Pool, runID, gate)
}

// RecordGate stores a run's merge gate verdict through db, so it can be
// written in the transaction that completes the run.
func RecordGate(ctx context.Context, db executor, runID int64, gate overload.Gate) error {
	data, err := json.Marshal(recordedGate{State: gate.State, HumanFiles: append([]string{}, gate.HumanFiles...), Held: gate.HeldBack})
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `UPDATE runs SET gate = $2 WHERE id = $1`, runID, data)
	return err
}

// GateHistory is what earlier reviews of a pull request mean for a run's
// merge gate.
type GateHistory struct {
	// HeldFailure is set when the latest earlier verdict on the same commit
	// failed and this run was not started by a person, so it cannot clear
	// that verdict.
	HeldFailure bool
	// PreviousHeld is set when that earlier verdict was itself held, so
	// the pull request was already told why the check stays failing.
	PreviousHeld bool
	// PostedHumanFiles are the human review path files of overload's last
	// posted review of the pull request; HumanFilesKnown is false when
	// there is none.
	PostedHumanFiles []string
	HumanFilesKnown  bool
}

// gateHistoryColumns selects what a run r's GateHistory is read from: who
// started it, the latest earlier verdict on the same commit, and the human
// files of the last posted review of the pull request.
const gateHistoryColumns = `r.trigger,
COALESCE((SELECT e.gate FROM runs e WHERE e.repository_id = r.repository_id AND e.pr_number = r.pr_number AND e.head_sha = r.head_sha AND e.id < r.id AND NOT e.dry_run AND e.gate <> '{}' ORDER BY e.id DESC LIMIT 1), 'null'),
COALESCE((SELECT e.gate->'human_files' FROM runs e WHERE e.repository_id = r.repository_id AND e.pr_number = r.pr_number AND e.id < r.id AND e.post_status IN ('posted', 'nothing_new') AND e.gate <> '{}' ORDER BY e.id DESC LIMIT 1), 'null')`

// gateHistoryRow holds the gateHistoryColumns of one run.
type gateHistoryRow struct {
	trigger              string
	earlier, postedHuman []byte
}

func (row *gateHistoryRow) targets() []any {
	return []any{&row.trigger, &row.earlier, &row.postedHuman}
}

func (row gateHistoryRow) history() (GateHistory, error) {
	var history GateHistory
	if string(row.earlier) != "null" {
		var earlier recordedGate
		if err := json.Unmarshal(row.earlier, &earlier); err != nil {
			return history, fmt.Errorf("read earlier verdict: %w", err)
		}
		history.HeldFailure = row.trigger != TriggerManual && earlier.State == overload.GateFailure
		history.PreviousHeld = earlier.Held
	}
	if string(row.postedHuman) != "null" {
		history.HumanFilesKnown = true
		if err := json.Unmarshal(row.postedHuman, &history.PostedHumanFiles); err != nil {
			return history, fmt.Errorf("read earlier human review files: %w", err)
		}
	}
	return history, nil
}

// LoadGateHistory returns the GateHistory of a run.
func (s *Store) LoadGateHistory(ctx context.Context, runID int64) (GateHistory, error) {
	return LoadGateHistory(ctx, s.Pool, runID)
}

// LoadGateHistory returns the GateHistory of a run through db.
func LoadGateHistory(ctx context.Context, db querier, runID int64) (GateHistory, error) {
	var row gateHistoryRow
	if err := db.QueryRow(ctx, `SELECT `+gateHistoryColumns+` FROM runs r WHERE r.id = $1`, runID).Scan(row.targets()...); err != nil {
		return GateHistory{}, err
	}
	return row.history()
}

// FailPost gives up on posting a run whose post job ran out of attempts:
// the post is marked failed, so it no longer holds back re-reviews, and a
// failing verdict is recorded, so automation cannot clear it.
func (s *Store) FailPost(ctx context.Context, runID int64, reason string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `UPDATE runs SET post_status = 'post_failed', gate = jsonb_build_object('state', 'failure', 'human_files', COALESCE(gate->'human_files', '[]'::jsonb)) WHERE id = $1 AND posted_at IS NULL AND post_status = 'queued'`, runID)
	if err != nil || command.RowsAffected() == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'error', 'posted', $2)`, runID, overload.TruncateUTF8("Posting failed: "+reason, 500)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
