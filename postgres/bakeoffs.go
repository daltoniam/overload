package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// MaxBakeoffRuns bounds the runs per model and case.
const MaxBakeoffRuns = 10

// BakeoffArgs is the job that runs a bake-off.
type BakeoffArgs struct {
	ID int64 `json:"id"`
}

func (BakeoffArgs) Kind() string { return "bakeoff" }

// Bakeoff is a bake-off and, once loaded with GetBakeoff, its results.
type Bakeoff struct {
	ID         int64
	Status     string
	Models     []overload.ModelProfile
	Suites     []eval.Suite
	Runs       int
	Error      string
	CreatedAt  time.Time
	FinishedAt *time.Time
	Results    []BakeoffResult
}

// BakeoffResult is one model's review of one case.
type BakeoffResult struct {
	Model        string
	Suite        string
	Case         string
	Run          int
	Score        eval.Score
	InputTokens  int64
	OutputTokens int64
	Duration     time.Duration
	Error        string
}

// SaveSuite creates or replaces a saved suite.
func (s *Store) SaveSuite(ctx context.Context, suite eval.Suite) error {
	if suite.Name == eval.BuiltinSuite {
		return fmt.Errorf("%s is the built-in suite's name", eval.BuiltinSuite)
	}
	if err := suite.Validate(); err != nil {
		return err
	}
	cases, err := json.Marshal(suite.Cases)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO bakeoff_suites (name, description, cases) VALUES ($1, $2, $3) ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description, cases = EXCLUDED.cases, updated_at = now()`, suite.Name, suite.Description, cases)
	return err
}

// ListSuites returns the built-in suite and the saved ones.
func (s *Store) ListSuites(ctx context.Context) ([]eval.Suite, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name, description, cases FROM bakeoff_suites ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	suites := []eval.Suite{eval.PlantedSuite()}
	for rows.Next() {
		var suite eval.Suite
		var cases []byte
		if err := rows.Scan(&suite.Name, &suite.Description, &cases); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(cases, &suite.Cases); err != nil {
			return nil, err
		}
		suites = append(suites, suite)
	}
	return suites, rows.Err()
}

// DeleteSuite deletes a saved suite.
func (s *Store) DeleteSuite(ctx context.Context, name string) error {
	command, err := s.Pool.Exec(ctx, `DELETE FROM bakeoff_suites WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CreateBakeoff pins the named models (saved model connections, which the
// model policy must allow) and suites and queues the bake-off.
func (s *Store) CreateBakeoff(ctx context.Context, client *river.Client[pgx.Tx], models, suites []string, runs int) (int64, error) {
	if len(models) == 0 || len(models) > 10 || len(suites) == 0 || runs < 1 || runs > MaxBakeoffRuns {
		return 0, fmt.Errorf("choose 1 to 10 models, at least one suite and 1 to %d runs", MaxBakeoffRuns)
	}
	settings, err := s.ListReviewSettings(ctx)
	if err != nil {
		return 0, err
	}
	policy, err := s.LoadModelPolicy(ctx)
	if err != nil {
		return 0, err
	}
	var profiles []overload.ModelProfile
	for _, name := range models {
		index := slices.IndexFunc(settings, func(setting overload.ReviewSettings) bool { return setting.Name == name })
		if index < 0 {
			return 0, fmt.Errorf("no model named %s", name)
		}
		if err := policy.Check(settings[index].Catalog, settings[index].Model).Error(); err != nil {
			return 0, fmt.Errorf("%w: %s: %v", ErrModelPolicy, name, err)
		}
		if !slices.ContainsFunc(profiles, func(profile overload.ModelProfile) bool { return profile.Name == name }) {
			profiles = append(profiles, settings[index].Profile())
		}
	}
	available, err := s.ListSuites(ctx)
	if err != nil {
		return 0, err
	}
	var pinned []eval.Suite
	for _, name := range suites {
		index := slices.IndexFunc(available, func(suite eval.Suite) bool { return suite.Name == name })
		if index < 0 {
			return 0, fmt.Errorf("no suite named %s", name)
		}
		pinned = append(pinned, available[index])
	}
	modelData, err := json.Marshal(profiles)
	if err != nil {
		return 0, err
	}
	suiteData, err := json.Marshal(pinned)
	if err != nil {
		return 0, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO bakeoffs (models, suites, runs) VALUES ($1, $2, $3) RETURNING id`, modelData, suiteData, runs).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := client.InsertTx(ctx, tx, BakeoffArgs{ID: id}, &river.InsertOpts{MaxAttempts: 1}); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// StartBakeoff marks a queued bake-off running and returns it; ok is false
// when it is not queued (already run, or cancelled).
func (s *Store) StartBakeoff(ctx context.Context, id int64) (Bakeoff, bool, error) {
	var bakeoff Bakeoff
	var models, suites []byte
	err := s.Pool.QueryRow(ctx, `UPDATE bakeoffs SET status = 'running', started_at = now() WHERE id = $1 AND status = 'queued' RETURNING id, models, suites, runs`, id).Scan(&bakeoff.ID, &models, &suites, &bakeoff.Runs)
	if errors.Is(err, pgx.ErrNoRows) {
		return bakeoff, false, nil
	}
	if err != nil {
		return bakeoff, false, err
	}
	if err := json.Unmarshal(models, &bakeoff.Models); err != nil {
		return bakeoff, false, err
	}
	if err := json.Unmarshal(suites, &bakeoff.Suites); err != nil {
		return bakeoff, false, err
	}
	return bakeoff, true, nil
}

// AddBakeoffResult stores one result.
func (s *Store) AddBakeoffResult(ctx context.Context, id int64, result BakeoffResult) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO bakeoff_results (bakeoff_id, model, suite, case_name, run, expected, matched, misplaced, reported, input_tokens, output_tokens, duration_ms, error) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (bakeoff_id, model, suite, case_name, run) DO NOTHING`, id, result.Model, result.Suite, result.Case, result.Run, result.Score.Expected, result.Score.Matched, result.Score.Misplaced, result.Score.Reported, result.InputTokens, result.OutputTokens, result.Duration.Milliseconds(), overload.TruncateUTF8(result.Error, 1000))
	return err
}

// FinishBakeoff records how a bake-off ended.
func (s *Store) FinishBakeoff(ctx context.Context, id int64, failure string) error {
	status := "completed"
	if failure != "" {
		status = "failed"
	}
	_, err := s.Pool.Exec(ctx, `UPDATE bakeoffs SET status = $2, error = $3, finished_at = now() WHERE id = $1`, id, status, overload.TruncateUTF8(failure, 1000))
	return err
}

// FailInterruptedBakeoffs ends bake-offs a restart interrupted; their job
// does not run again.
func (s *Store) FailInterruptedBakeoffs(ctx context.Context) (int64, error) {
	command, err := s.Pool.Exec(ctx, `UPDATE bakeoffs SET status = 'failed', error = 'interrupted by a restart; start it again', finished_at = now() WHERE status = 'running'`)
	return command.RowsAffected(), err
}

// ListBakeoffs returns recent bake-offs, newest first, without results.
func (s *Store) ListBakeoffs(ctx context.Context) ([]Bakeoff, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, status, models, suites, runs, error, created_at, finished_at FROM bakeoffs ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bakeoffs []Bakeoff
	for rows.Next() {
		bakeoff, err := scanBakeoff(rows)
		if err != nil {
			return nil, err
		}
		bakeoffs = append(bakeoffs, bakeoff)
	}
	return bakeoffs, rows.Err()
}

func scanBakeoff(row pgx.Row) (Bakeoff, error) {
	var bakeoff Bakeoff
	var models, suites []byte
	if err := row.Scan(&bakeoff.ID, &bakeoff.Status, &models, &suites, &bakeoff.Runs, &bakeoff.Error, &bakeoff.CreatedAt, &bakeoff.FinishedAt); err != nil {
		return bakeoff, err
	}
	if err := json.Unmarshal(models, &bakeoff.Models); err != nil {
		return bakeoff, err
	}
	if err := json.Unmarshal(suites, &bakeoff.Suites); err != nil {
		return bakeoff, err
	}
	return bakeoff, nil
}

// GetBakeoff returns a bake-off with its results.
func (s *Store) GetBakeoff(ctx context.Context, id int64) (Bakeoff, error) {
	bakeoff, err := scanBakeoff(s.Pool.QueryRow(ctx, `SELECT id, status, models, suites, runs, error, created_at, finished_at FROM bakeoffs WHERE id = $1`, id))
	if err != nil {
		return bakeoff, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT model, suite, case_name, run, expected, matched, misplaced, reported, input_tokens, output_tokens, duration_ms, error FROM bakeoff_results WHERE bakeoff_id = $1 ORDER BY id`, id)
	if err != nil {
		return bakeoff, err
	}
	defer rows.Close()
	for rows.Next() {
		var result BakeoffResult
		var duration int64
		if err := rows.Scan(&result.Model, &result.Suite, &result.Case, &result.Run, &result.Score.Expected, &result.Score.Matched, &result.Score.Misplaced, &result.Score.Reported, &result.InputTokens, &result.OutputTokens, &duration, &result.Error); err != nil {
			return bakeoff, err
		}
		result.Duration = time.Duration(duration) * time.Millisecond
		result.Score.Unmatched = result.Score.Reported - result.Score.Matched
		bakeoff.Results = append(bakeoff.Results, result)
	}
	return bakeoff, rows.Err()
}
