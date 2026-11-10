package postgres

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// Bake-off limits: runs per model and case, models, and reviews in total
// (models times cases times runs), which bounds how long one can occupy
// the bake-off queue.
const (
	MaxBakeoffRuns    = 10
	MaxBakeoffModels  = 10
	MaxBakeoffReviews = 300
)

// BakeoffQueue is the River queue bake-offs run on, one at a time, so a
// bake-off lasting hours never holds a slot pull request reviews need.
const BakeoffQueue = "bakeoffs"

// BakeoffInterrupted is a bake-off's error when the server stopped while it
// ran.
const BakeoffInterrupted = "interrupted by a restart; start it again"

// BakeoffArgs is the job that runs a bake-off.
type BakeoffArgs struct {
	ID int64 `json:"id"`
}

func (BakeoffArgs) Kind() string { return "bakeoff" }

// BakeoffModel is a model connection pinned for a bake-off, with the
// built-in prompt the connection reviews with.
type BakeoffModel struct {
	overload.ModelProfile
	PromptProfile string `json:"prompt_profile,omitempty"`
}

// Prompt is the built-in prompt the model reviews with.
func (model BakeoffModel) Prompt() string {
	return cmp.Or(model.PromptProfile, "context")
}

// Bakeoff is a bake-off and, once loaded with GetBakeoff, its results. It
// holds only the names of its pinned suites; BakeoffPlan holds the cases.
type Bakeoff struct {
	ID         int64
	Status     string
	Models     []BakeoffModel
	SuiteNames []string
	Runs       int
	Error      string
	CreatedAt  time.Time
	Results    []BakeoffResult
}

// BakeoffPlan is what a started bake-off runs.
type BakeoffPlan struct {
	ID     int64
	Models []BakeoffModel
	Suites []eval.Suite
	Runs   int
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

// SuiteSummary describes a suite without loading its cases.
type SuiteSummary struct {
	Name        string
	Description string
	Cases       int
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

// ListSuites summarizes the built-in suite and the saved ones.
func (s *Store) ListSuites(ctx context.Context) ([]SuiteSummary, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name, description, jsonb_array_length(cases) FROM bakeoff_suites ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	planted := eval.PlantedSuite()
	suites := []SuiteSummary{{Name: planted.Name, Description: planted.Description, Cases: len(planted.Cases)}}
	for rows.Next() {
		var suite SuiteSummary
		if err := rows.Scan(&suite.Name, &suite.Description, &suite.Cases); err != nil {
			return nil, err
		}
		suites = append(suites, suite)
	}
	return suites, rows.Err()
}

// loadSuites returns the named suites with their cases, in the order named.
func (s *Store) loadSuites(ctx context.Context, names []string) ([]eval.Suite, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name, description, cases FROM bakeoff_suites WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	saved := map[string]eval.Suite{}
	for rows.Next() {
		var suite eval.Suite
		var cases []byte
		if err := rows.Scan(&suite.Name, &suite.Description, &cases); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(cases, &suite.Cases); err != nil {
			return nil, fmt.Errorf("suite %s: %w", suite.Name, err)
		}
		saved[suite.Name] = suite
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	suites := make([]eval.Suite, 0, len(names))
	for _, name := range names {
		suite, ok := saved[name]
		if name == eval.BuiltinSuite {
			suite, ok = eval.PlantedSuite(), true
		}
		if !ok {
			return nil, fmt.Errorf("no suite named %s", name)
		}
		suites = append(suites, suite)
	}
	return suites, nil
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

// uniqueNames trims names and drops empty and repeated ones.
func uniqueNames(names []string) []string {
	var unique []string
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(unique, name) {
			unique = append(unique, name)
		}
	}
	return unique
}

// CreateBakeoff pins the named models (saved model connections, which the
// model policy must allow) and suites and queues the bake-off.
func (s *Store) CreateBakeoff(ctx context.Context, client *river.Client[pgx.Tx], models, suites []string, runs int) (int64, error) {
	models, suites = uniqueNames(models), uniqueNames(suites)
	if len(models) == 0 || len(models) > MaxBakeoffModels || len(suites) == 0 || runs < 1 || runs > MaxBakeoffRuns {
		return 0, fmt.Errorf("choose 1 to %d models, at least one suite and 1 to %d runs", MaxBakeoffModels, MaxBakeoffRuns)
	}
	settings, err := s.ListReviewSettings(ctx)
	if err != nil {
		return 0, err
	}
	policy, err := s.LoadModelPolicy(ctx)
	if err != nil {
		return 0, err
	}
	var pinnedModels []BakeoffModel
	for _, name := range models {
		index := slices.IndexFunc(settings, func(setting overload.ReviewSettings) bool { return setting.Name == name })
		if index < 0 {
			return 0, fmt.Errorf("no model named %s", name)
		}
		if err := policy.Check(settings[index].Catalog, settings[index].Model).Error(); err != nil {
			return 0, fmt.Errorf("%w: %s: %v", ErrModelPolicy, name, err)
		}
		pinnedModels = append(pinnedModels, BakeoffModel{ModelProfile: settings[index].Profile(), PromptProfile: settings[index].PromptProfile})
	}
	pinnedSuites, err := s.loadSuites(ctx, suites)
	if err != nil {
		return 0, err
	}
	cases := 0
	for _, suite := range pinnedSuites {
		cases += len(suite.Cases)
	}
	if reviews := len(pinnedModels) * cases * runs; reviews > MaxBakeoffReviews {
		return 0, fmt.Errorf("that is %d reviews; choose fewer models, cases or runs (at most %d reviews)", reviews, MaxBakeoffReviews)
	}
	modelData, err := json.Marshal(pinnedModels)
	if err != nil {
		return 0, err
	}
	suiteData, err := json.Marshal(pinnedSuites)
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
	// Retrying is safe: only an attempt that finds the bake-off queued runs
	// it, so the extra attempts cover a failure to start.
	if _, err := client.InsertTx(ctx, tx, BakeoffArgs{ID: id}, &river.InsertOpts{Queue: BakeoffQueue, MaxAttempts: 3}); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// StartBakeoff marks a queued bake-off running and returns what it runs; ok
// is false when it is not queued (already run, or running).
func (s *Store) StartBakeoff(ctx context.Context, id int64) (BakeoffPlan, bool, error) {
	plan := BakeoffPlan{ID: id}
	var models, suites []byte
	err := s.Pool.QueryRow(ctx, `UPDATE bakeoffs SET status = 'running', started_at = now() WHERE id = $1 AND status = 'queued' RETURNING models, suites, runs`, id).Scan(&models, &suites, &plan.Runs)
	if errors.Is(err, pgx.ErrNoRows) {
		return plan, false, nil
	}
	if err != nil {
		return plan, false, err
	}
	decodeErr := json.Unmarshal(models, &plan.Models)
	if decodeErr == nil {
		decodeErr = json.Unmarshal(suites, &plan.Suites)
	}
	if decodeErr != nil {
		// A retry would find the bake-off running, so end it here.
		if err := s.FinishBakeoff(ctx, id, "the pinned models or suites could not be read"); err != nil {
			return plan, false, err
		}
		return plan, false, fmt.Errorf("read bake-off %d: %w", id, decodeErr)
	}
	return plan, true, nil
}

// AddBakeoffResult stores one result.
func (s *Store) AddBakeoffResult(ctx context.Context, id int64, result BakeoffResult) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO bakeoff_results (bakeoff_id, model, suite, case_name, run, expected, matched, misplaced, reported, unmatched, placement_checked, input_tokens, output_tokens, duration_ms, error) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (bakeoff_id, model, suite, case_name, run) DO NOTHING`, id, result.Model, result.Suite, result.Case, result.Run, result.Score.Expected, result.Score.Matched, result.Score.Misplaced, result.Score.Reported, result.Score.Unmatched, result.Score.PlacementChecked, result.InputTokens, result.OutputTokens, result.Duration.Milliseconds(), overload.TruncateUTF8(result.Error, 1000))
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
// does not run them again.
func (s *Store) FailInterruptedBakeoffs(ctx context.Context) (int64, error) {
	command, err := s.Pool.Exec(ctx, `UPDATE bakeoffs SET status = 'failed', error = $1, finished_at = now() WHERE status = 'running'`, BakeoffInterrupted)
	return command.RowsAffected(), err
}

// bakeoffColumns selects a bake-off's suite names rather than its pinned
// cases, which can be megabytes.
const bakeoffColumns = `id, status, models, jsonb_path_query_array(suites, '$[*].name'), runs, error, created_at`

// ListBakeoffs returns recent bake-offs, newest first, without results.
func (s *Store) ListBakeoffs(ctx context.Context) ([]Bakeoff, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+bakeoffColumns+` FROM bakeoffs ORDER BY id DESC LIMIT 100`)
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
	var models, suiteNames []byte
	if err := row.Scan(&bakeoff.ID, &bakeoff.Status, &models, &suiteNames, &bakeoff.Runs, &bakeoff.Error, &bakeoff.CreatedAt); err != nil {
		return bakeoff, err
	}
	if err := json.Unmarshal(models, &bakeoff.Models); err != nil {
		return bakeoff, err
	}
	if err := json.Unmarshal(suiteNames, &bakeoff.SuiteNames); err != nil {
		return bakeoff, err
	}
	return bakeoff, nil
}

// GetBakeoff returns a bake-off with its results.
func (s *Store) GetBakeoff(ctx context.Context, id int64) (Bakeoff, error) {
	bakeoff, err := scanBakeoff(s.Pool.QueryRow(ctx, `SELECT `+bakeoffColumns+` FROM bakeoffs WHERE id = $1`, id))
	if err != nil {
		return bakeoff, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT model, suite, case_name, run, expected, matched, misplaced, reported, unmatched, placement_checked, input_tokens, output_tokens, duration_ms, error FROM bakeoff_results WHERE bakeoff_id = $1 ORDER BY id`, id)
	if err != nil {
		return bakeoff, err
	}
	defer rows.Close()
	for rows.Next() {
		var result BakeoffResult
		var duration int64
		if err := rows.Scan(&result.Model, &result.Suite, &result.Case, &result.Run, &result.Score.Expected, &result.Score.Matched, &result.Score.Misplaced, &result.Score.Reported, &result.Score.Unmatched, &result.Score.PlacementChecked, &result.InputTokens, &result.OutputTokens, &duration, &result.Error); err != nil {
			return bakeoff, err
		}
		result.Duration = time.Duration(duration) * time.Millisecond
		if result.Score.Expected > 0 {
			result.Score.Recall = float64(result.Score.Matched) / float64(result.Score.Expected)
		}
		bakeoff.Results = append(bakeoff.Results, result)
	}
	return bakeoff, rows.Err()
}
