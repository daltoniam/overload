package overload

import (
	"errors"
	"fmt"
	"strings"

	"github.com/daltoniam/overload/diff"
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

// GateFiles are a pull request's changed files as the host read them from
// its diff. Changed holds every name the diff touches, including deleted,
// binary and renamed-away files; Commentable the files with lines a review
// can comment on, which the routing must cover.
type GateFiles struct {
	Changed     []string `json:"changed"`
	Commentable []string `json:"commentable"`
}

// GateFilesFromDiff reads a pull request diff's changed files for the
// merge gate, independently of what a sandboxed review reports.
func GateFilesFromDiff(patch string) (GateFiles, error) {
	changed, err := diff.ChangedPaths(patch)
	if err != nil {
		return GateFiles{}, err
	}
	parsed, err := diff.Parse(patch)
	if err != nil {
		return GateFiles{}, err
	}
	commentable := make([]string, 0, len(parsed))
	for _, file := range parsed {
		commentable = append(commentable, file.Path)
	}
	return GateFiles{Changed: changed, Commentable: uniqueSorted(commentable)}, nil
}

// GateInput is what decides a gate.
type GateInput struct {
	Decision, BlockSeverity string
	HumanReviewPaths        []string
	ApprovePaths            []string
	SkipPaths               []string
	// Files are the changed files read from the diff on the host; nil
	// means they are unknown, which fails the gate.
	Files   *GateFiles
	Routing Routing
	// Degraded is set when the review completed partially for any reason.
	Degraded bool
	// Severities are those of every finding the review stands for,
	// including ones posted on earlier commits.
	Severities []string
	// NotPosted is set when the review is not posted to GitHub, so the
	// status must not claim it approved or requested changes.
	NotPosted bool
}

// GateOutcome decides a finished review's event and commit status. It
// never fails open:
//   - a review whose changed files are unknown, or that left any
//     commentable file unrouted or unreviewed (too large, or an agent
//     failed), sets a failing status, so a required "overload" check cannot
//     pass on files nobody reviewed;
//   - a review never approves when it reviewed no file, when any changed
//     name (old and new names, deleted and binary files included) is on a
//     human review path, or when the workflow names approve paths and any
//     changed name is outside them;
//   - blocking findings request changes and fail the status.
func GateOutcome(input GateInput) Gate {
	var gate Gate
	var changed []string
	reviewed := 0
	if input.Files != nil {
		changed = input.Files.Changed
		gate.Unreviewed, reviewed = coverage(input.Routing, *input.Files, input.SkipPaths)
	}
	partial := input.Degraded || input.Files == nil || len(gate.Unreviewed) > 0
	gate.Event = ReviewEvent(input.Decision, input.BlockSeverity, partial, input.Severities)
	human, humanErr := compileHumanGlobs(input.HumanReviewPaths)
	for _, path := range changed {
		if humanErr == nil && human.matchFileOrFolder(path) {
			gate.HumanFiles = append(gate.HumanFiles, path)
		}
	}
	approvable := humanErr == nil && len(gate.HumanFiles) == 0 && reviewed > 0 && allMatch(input.ApprovePaths, changed)
	if gate.Event == ReviewApprove && !approvable {
		gate.Event = ReviewComment
	}
	switch {
	case input.Files == nil:
		gate.State, gate.Description = GateFailure, "Review did not record its changed files"
	case len(gate.Unreviewed) > 0:
		gate.State, gate.Description = GateFailure, fmt.Sprintf("Partial review: %d file(s) not reviewed", len(gate.Unreviewed))
	case gate.Event == ReviewRequestChanges && input.NotPosted:
		gate.State, gate.Description = GateFailure, "Would request changes: blocking issues found"
	case gate.Event == ReviewRequestChanges:
		gate.State, gate.Description = GateFailure, "Changes requested: blocking issues found"
	case gate.Event == ReviewApprove && input.NotPosted:
		gate.State, gate.Description = GateSuccess, "Would approve"
	case gate.Event == ReviewApprove:
		gate.State, gate.Description = GateSuccess, "Approved"
	case len(input.Severities) == 0:
		gate.State, gate.Description = GateSuccess, "No issues found"
	default:
		gate.State, gate.Description = GateSuccess, fmt.Sprintf("%d issue(s) noted as comments", len(input.Severities))
	}
	if humanErr != nil {
		gate.Description += "; human review paths are invalid, so no approval"
	}
	if len(gate.HumanFiles) > 0 {
		gate.Description += fmt.Sprintf("; %d sensitive file(s) need a person's review", len(gate.HumanFiles))
	}
	if input.Degraded && input.Files != nil && len(gate.Unreviewed) == 0 {
		gate.Description = "Partial review. " + gate.Description
	}
	if input.NotPosted {
		gate.Description += " (not posted: posting is off)"
	}
	return gate
}

// coverage checks a sandbox's routing against the commentable files the
// host read from the diff. A file counts as reviewed when an agent was
// given it and did not fail; skipped files must match the workflow's skip
// paths; any other file, including one the routing never mentions, is
// unreviewed.
func coverage(routing Routing, files GateFiles, skipPaths []string) ([]string, int) {
	// Invalid skip paths skip nothing.
	skip, _ := compileGlobs(skipPaths)
	skipped := map[string]bool{}
	for _, path := range routing.Skipped {
		skipped[path] = skip.match(path)
	}
	failed := map[string]bool{}
	for _, path := range routing.UnreviewedFiles() {
		failed[path] = true
	}
	assigned := map[string]bool{}
	for _, agent := range routing.Agents {
		for _, path := range agent.Files {
			assigned[path] = true
		}
	}
	unreviewed := routing.UnreviewedFiles()
	reviewed := 0
	for _, path := range files.Commentable {
		switch {
		case skipped[path]:
		case assigned[path] && !failed[path]:
			reviewed++
		default:
			unreviewed = append(unreviewed, path)
		}
	}
	return uniqueSorted(unreviewed), reviewed
}

// allMatch reports whether every changed name matches globs; with no globs,
// any file may be approved.
func allMatch(globs, changed []string) bool {
	if len(globs) == 0 {
		return true
	}
	set, err := compileGlobs(globs)
	if err != nil {
		return false
	}
	for _, path := range changed {
		if !set.match(path) {
			return false
		}
	}
	return true
}

// compileHumanGlobs compiles human review paths to match regardless of
// case, since a sensitive folder renamed to "Auth" is no less sensitive.
func compileHumanGlobs(globs []string) (globSet, error) {
	lowered := make([]string, len(globs))
	for index, glob := range globs {
		lowered[index] = strings.ToLower(glob)
	}
	return compileGlobs(lowered)
}

// matchFileOrFolder matches a path or any folder above it, so a bare
// "auth" (which compiles to a file name pattern) also covers every file in
// an auth folder.
func (set globSet) matchFileOrFolder(path string) bool {
	candidate := strings.ToLower(path)
	for {
		if set.match(candidate) {
			return true
		}
		index := strings.LastIndex(candidate, "/")
		if index < 0 {
			return false
		}
		candidate = candidate[:index]
	}
}
