package overload

import "errors"

// Review decisions a PR workflow can post with its findings.
const (
	// ReviewDecisionComment posts findings as comments only (the default).
	ReviewDecisionComment = "comment"
	// ReviewDecisionRequestChanges requests changes when a finding is at or
	// above the workflow's block severity.
	ReviewDecisionRequestChanges = "request_changes"
	// ReviewDecisionApprove also approves a complete review with nothing
	// blocking.
	ReviewDecisionApprove = "approve"
)

// GitHub review events.
const (
	ReviewComment        = "COMMENT"
	ReviewRequestChanges = "REQUEST_CHANGES"
	ReviewApprove        = "APPROVE"
)

// DefaultBlockSeverity is the lowest severity that requests changes when a
// workflow does not set one.
const DefaultBlockSeverity = "high"

var severityOrder = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}

func validateReviewDecision(kind, decision, block string) error {
	if decision == "" && block == "" {
		return nil
	}
	if kind != "pr_review" {
		return errors.New("only PR review workflows approve or request changes")
	}
	switch decision {
	case ReviewDecisionComment, ReviewDecisionRequestChanges, ReviewDecisionApprove:
	case "":
		return errors.New("block_severity needs review_decision request_changes or approve")
	default:
		return errors.New("review_decision must be comment, request_changes or approve")
	}
	if _, ok := severityOrder[block]; block != "" && !ok {
		return errors.New("block_severity must be critical, high, medium or low")
	}
	if block != "" && decision == ReviewDecisionComment {
		return errors.New("block_severity needs review_decision request_changes or approve")
	}
	return nil
}

// ReviewEvent picks the GitHub review event for a review. severities are
// those of every finding the review stands for, including ones already
// posted on earlier commits. A partial review never approves.
func ReviewEvent(decision, block string, partial bool, severities []string) string {
	if decision != ReviewDecisionRequestChanges && decision != ReviewDecisionApprove {
		return ReviewComment
	}
	threshold, ok := severityOrder[block]
	if !ok {
		threshold = severityOrder[DefaultBlockSeverity]
	}
	for _, severity := range severities {
		if rank, known := severityOrder[severity]; known && rank <= threshold {
			return ReviewRequestChanges
		}
	}
	if decision == ReviewDecisionApprove && !partial {
		return ReviewApprove
	}
	return ReviewComment
}
