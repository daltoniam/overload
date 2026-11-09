package overload

import (
	"errors"
	"fmt"
	"slices"
)

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

func validateGatePaths(kind, decision string, human, approve []string) error {
	if len(human) == 0 && len(approve) == 0 {
		return nil
	}
	if kind != "pr_review" {
		return errors.New("only PR review workflows have human review or approval paths")
	}
	if len(approve) > 0 && decision != ReviewDecisionApprove {
		return errors.New("approve_paths need review_decision approve")
	}
	if err := validateGlobs(human); err != nil {
		return fmt.Errorf("human review paths: %w", err)
	}
	if err := validateGlobs(approve); err != nil {
		return fmt.Errorf("approve paths: %w", err)
	}
	return nil
}

// Commit status states for a review's merge gate.
const (
	GateSuccess = "success"
	GateFailure = "failure"
)

// Gate is how a finished review stands as a merge gate: the GitHub review
// event to post and the commit status to set.
type Gate struct {
	Event       string
	State       string
	Description string
	// HumanFiles are changed files under the workflow's human review
	// paths; Unreviewed are changed files no review covered.
	HumanFiles []string
	Unreviewed []string
}

// GateInput is what decides a gate.
type GateInput struct {
	Decision, BlockSeverity string
	HumanReviewPaths        []string
	ApprovePaths            []string
	Routing                 Routing
	// Degraded is set when the review completed partially for any reason.
	Degraded bool
	// Severities are those of every finding the review stands for,
	// including ones posted on earlier commits.
	Severities []string
}

// GateOutcome decides a finished review's event and commit status. It
// never fails open:
//   - a review that left any changed file unreviewed (too large, or an
//     agent failed) sets a failing status, so a required "overload" check
//     cannot pass on files nobody reviewed;
//   - a review never approves when it touches a human review path, or when
//     the workflow names approve paths and a changed file is outside them;
//   - blocking findings request changes and fail the status as before.
func GateOutcome(input GateInput) Gate {
	gate := Gate{Unreviewed: input.Routing.UnreviewedFiles()}
	partial := input.Degraded || len(gate.Unreviewed) > 0
	gate.Event = ReviewEvent(input.Decision, input.BlockSeverity, partial, input.Severities)
	changed := input.Routing.ChangedFiles()
	if human, err := compileGlobs(input.HumanReviewPaths); err == nil {
		for _, path := range changed {
			if human.match(path) {
				gate.HumanFiles = append(gate.HumanFiles, path)
			}
		}
	}
	if gate.Event == ReviewApprove && (len(gate.HumanFiles) > 0 || !allMatch(input.ApprovePaths, changed, input.Routing.Skipped)) {
		gate.Event = ReviewComment
	}
	switch {
	case len(gate.Unreviewed) > 0:
		gate.State, gate.Description = GateFailure, fmt.Sprintf("Partial review: %d file(s) not reviewed", len(gate.Unreviewed))
	case gate.Event == ReviewRequestChanges:
		gate.State, gate.Description = GateFailure, "Changes requested: blocking issues found"
	case gate.Event == ReviewApprove:
		gate.State, gate.Description = GateSuccess, "Approved"
	case len(input.Severities) == 0:
		gate.State, gate.Description = GateSuccess, "No issues found"
	default:
		gate.State, gate.Description = GateSuccess, fmt.Sprintf("%d issue(s) noted as comments", len(input.Severities))
	}
	if len(gate.HumanFiles) > 0 {
		gate.Description += fmt.Sprintf("; %d sensitive file(s) need a person's review", len(gate.HumanFiles))
	}
	if input.Degraded && len(gate.Unreviewed) == 0 {
		gate.Description = "Partial review. " + gate.Description
	}
	return gate
}

// allMatch reports whether every changed file that was not skipped matches
// globs; with no globs, any file may be approved.
func allMatch(globs, changed, skipped []string) bool {
	if len(globs) == 0 {
		return true
	}
	set, err := compileGlobs(globs)
	if err != nil {
		return false
	}
	for _, path := range changed {
		if !slices.Contains(skipped, path) && !set.match(path) {
			return false
		}
	}
	return true
}
