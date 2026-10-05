package postgres

import (
	"context"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// FindingDropped is the status of findings the verifier dropped. They are
// kept on the run but never posted.
const FindingDropped = "dropped"

// InsertFindings stores a run's findings with their fingerprints, which
// identify the same finding across runs of the pull request, and the agents
// that reported each one. Findings with a drop reason are stored as
// dropped whatever status is given.
func InsertFindings(ctx context.Context, tx pgx.Tx, runID int64, repository string, pr int, status string, findings []overload.Finding) error {
	for _, finding := range findings {
		agents := finding.Agents
		if agents == nil {
			agents = []string{}
		}
		findingStatus := status
		if finding.DropReason != "" {
			findingStatus = FindingDropped
		}
		if _, err := tx.Exec(ctx, `INSERT INTO findings (run_id, path, line, start_line, side, severity, category, title, body, confidence, evidence, status, fingerprint, agents, suppressed_reason) VALUES ($1, $2, $3, NULLIF($4, 0), $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`, runID, finding.Path, finding.Line, finding.StartLine, finding.Side, finding.Severity, finding.Category, finding.Title, finding.Body, finding.Confidence, finding.Evidence, findingStatus, overload.FindingFingerprint(repository, pr, finding), agents, overload.TruncateUTF8(finding.DropReason, 500)); err != nil {
			return err
		}
	}
	return nil
}
