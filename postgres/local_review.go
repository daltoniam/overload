package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

func (s *Store) StartLocalReview(ctx context.Context, repo string, pr int, workflow overload.ResolvedWorkflow) (int64, error) {
	if repo == "" || pr <= 0 {
		return 0, errors.New("repository and positive PR number required")
	}
	if len(workflow.Agents) == 0 {
		return 0, errors.New("workflow has no agents")
	}
	profileData, err := json.Marshal(workflow.Agents[0].Model)
	if err != nil {
		return 0, err
	}
	snapshot, err := json.Marshal(workflow)
	if err != nil {
		return 0, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var repoID int64
	if err := tx.QueryRow(ctx, `INSERT INTO repositories (full_name) VALUES ($1) ON CONFLICT (full_name) DO UPDATE SET full_name = EXCLUDED.full_name RETURNING id`, repo).Scan(&repoID); err != nil {
		return 0, err
	}
	var runID int64
	if err := tx.QueryRow(ctx, `INSERT INTO runs (repository_id, pr_number, trigger, status, model_profile, config_snapshot, dry_run, started_at) VALUES ($1, $2, 'cli_inline', 'running', $3, $4, true, now()) RETURNING id`, repoID, pr, profileData, snapshot).Scan(&runID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, 'info', 'running', 'Local PR review started')`, runID); err != nil {
		return 0, err
	}
	return runID, tx.Commit(ctx)
}

func (s *Store) FinishLocalReview(ctx context.Context, runID int64, spec overload.ReviewSpec, result overload.ReviewResult, headSHA string, reviewErr error) error {
	specData, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	resultData, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	status := "completed"
	errorCode, errorMessage := "", ""
	if reviewErr != nil || result.Error != "" {
		status = "failed"
		errorCode = "review_failed"
		errorMessage = result.Error
		if reviewErr != nil {
			errorMessage = reviewErr.Error()
		}
		errorMessage = overload.TruncateUTF8(errorMessage, 2000)
	}
	metrics, err := json.Marshal(result.Metrics)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE runs SET status=$1, head_sha=$2, prompt_version=$3, error_code=$4, error_message=$5, metrics=COALESCE($6::jsonb, '{}'::jsonb), finished_at=now() WHERE id=$7 AND status='running'`, status, headSHA, spec.Workflow.Name, errorCode, errorMessage, metrics, runID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return errors.New("run is not in progress")
	}
	for name, content := range map[string][]byte{"spec.json": specData, "result.json": resultData} {
		if _, err := tx.Exec(ctx, `INSERT INTO run_artifacts (run_id, name, content) VALUES ($1, $2, $3)`, runID, name, content); err != nil {
			return fmt.Errorf("save %s: %w", name, err)
		}
	}
	for _, finding := range result.Findings {
		if _, err := tx.Exec(ctx, `INSERT INTO findings (run_id, path, line, start_line, side, severity, category, title, body, confidence, evidence, fingerprint, status) VALUES ($1, $2, $3, NULLIF($4, 0), $5, $6, $7, $8, $9, $10, $11, $12, 'dry_run')`, runID, finding.Path, finding.Line, finding.StartLine, finding.Side, finding.Severity, finding.Category, finding.Title, finding.Body, finding.Confidence, finding.Evidence, overload.FindingFingerprint(spec.Repository.FullName, spec.PRNumber, finding)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, level, step, message) VALUES ($1, $2, $3, $4)`, runID, map[bool]string{true: "error", false: "info"}[status == "failed"], status, map[bool]string{true: errorMessage, false: "Local PR review completed"}[status == "failed"]); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListFindings(ctx context.Context, runID int64) ([]overload.Finding, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, path, line, COALESCE(start_line, 0), side, severity, category, title, body, confidence, evidence FROM findings WHERE run_id=$1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var findings []overload.Finding
	for rows.Next() {
		var finding overload.Finding
		finding.RunID = runID
		if err := rows.Scan(&finding.ID, &finding.Path, &finding.Line, &finding.StartLine, &finding.Side, &finding.Severity, &finding.Category, &finding.Title, &finding.Body, &finding.Confidence, &finding.Evidence); err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}
	return findings, rows.Err()
}

func (s *Store) ReadArtifact(ctx context.Context, runID int64, name string) ([]byte, error) {
	var content []byte
	err := s.Pool.QueryRow(ctx, `SELECT content FROM run_artifacts WHERE run_id=$1 AND name=$2`, runID, name).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("artifact %s not found", name)
	}
	return content, err
}
