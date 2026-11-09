// Package checkout reviews and fixes changes in a local git checkout,
// without GitHub: the diff is the branch's changes against a base, and the
// repository is the working tree.
package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/harness"
)

var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@{}^~-]{0,199}$`)

// Git runs git in dir and returns its standard output.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// Diff returns the changes in dir since it branched from base: committed
// and uncommitted changes to tracked files. Untracked files are left out.
func Diff(ctx context.Context, dir, base string) (string, error) {
	if !refPattern.MatchString(base) || strings.Contains(base, "..") {
		return "", errors.New("invalid base reference")
	}
	mergeBase, err := Git(ctx, dir, "merge-base", base, "HEAD")
	if err != nil {
		return "", err
	}
	diff, err := Git(ctx, dir, "diff", "--no-color", "--no-ext-diff", strings.TrimSpace(mergeBase))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(diff) == "" {
		return "", fmt.Errorf("no changes since %s", base)
	}
	return diff, nil
}

// Review reviews dir's changes since base with workflow.
func Review(ctx context.Context, dir, base string, workflow overload.ResolvedWorkflow) (overload.ReviewResult, error) {
	diff, err := Diff(ctx, dir, base)
	if err != nil {
		return overload.ReviewResult{}, err
	}
	return harness.Reviewer{}.Review(ctx, overload.ReviewSpec{Diff: diff, Workflow: workflow}, os.DirFS(dir))
}

// Routing reads a result's routing.
func Routing(result overload.ReviewResult) overload.Routing {
	routing, _ := result.Metrics["routing"].(overload.Routing)
	return routing
}

// Gate is the merge gate a checkout review would set.
func Gate(workflow overload.ResolvedWorkflow, result overload.ReviewResult) overload.Gate {
	degraded, _ := result.Metrics["degraded"].(bool)
	severities := make([]string, 0, len(result.Findings))
	for _, finding := range result.Findings {
		severities = append(severities, finding.Severity)
	}
	return overload.GateOutcome(overload.GateInput{Decision: workflow.ReviewDecision, BlockSeverity: workflow.BlockSeverity, HumanReviewPaths: workflow.HumanReviewPaths, ApprovePaths: workflow.ApprovePaths, Routing: Routing(result), Degraded: degraded, Severities: severities})
}

// Report writes a review as Markdown.
func Report(w io.Writer, workflow overload.ResolvedWorkflow, result overload.ReviewResult) error {
	gate := Gate(workflow, result)
	routing := Routing(result)
	var out strings.Builder
	fmt.Fprintf(&out, "# Review: %s\n\n", gate.Description)
	if routing.Tier != "" {
		fmt.Fprintf(&out, "Risk tier: %s.\n", routing.Tier)
	}
	for _, agent := range routing.Agents {
		switch {
		case agent.OutOfTier:
			fmt.Fprintf(&out, "- %s: not needed for a %s change\n", agent.Agent, routing.Tier)
		case len(agent.Files) > 0:
			fmt.Fprintf(&out, "- %s reviewed %d of %s, %s\n", agent.Agent, agent.Reviewed, overload.Plural(len(agent.Files), "file"), overload.Plural(agent.Findings, "finding"))
		}
	}
	for _, note := range routing.Degraded {
		fmt.Fprintf(&out, "- Partial: %s\n", note)
	}
	if len(gate.HumanFiles) > 0 {
		fmt.Fprintf(&out, "- Needs a person: %s\n", strings.Join(gate.HumanFiles, ", "))
	}
	out.WriteString("\n")
	if len(result.Findings) == 0 {
		out.WriteString("No findings. This does not prove the change is bug-free.\n")
	}
	for _, finding := range result.Findings {
		fmt.Fprintf(&out, "## %s:%d [%s] %s\n\n%s\n\n", finding.Path, finding.Line, finding.Severity, finding.Title, finding.Body)
		if finding.Evidence != "" {
			fmt.Fprintf(&out, "```\n%s\n```\n\n", finding.Evidence)
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}
