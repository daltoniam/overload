package postgres

import (
	"context"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// InsertFindings stores a run's findings with their fingerprints, which
// identify the same finding across runs of the pull request, and the agents
// that reported each one.
func InsertFindings(ctx context.Context, tx pgx.Tx, runID int64, repository string, pr int, status string, findings []overload.Finding) error {
	for _, finding := range findings {
		agents := finding.Agents
		if agents == nil {
			agents = []string{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO findings (run_id, path, line, start_line, side, severity, category, title, body, confidence, evidence, status, fingerprint, agents) VALUES ($1, $2, $3, NULLIF($4, 0), $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`, runID, finding.Path, finding.Line, finding.StartLine, finding.Side, finding.Severity, finding.Category, finding.Title, finding.Body, finding.Confidence, finding.Evidence, status, overload.FindingFingerprint(repository, pr, finding), agents); err != nil {
			return err
		}
	}
	return nil
}
