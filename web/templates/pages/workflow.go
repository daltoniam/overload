package pages

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/daltoniam/overload"
)

// PreviewForm is what the workflow preview page was asked to route: a pasted
// file list, or a pull request when the server can fetch one.
type PreviewForm struct {
	Files        string
	Repository   string
	PullRequest  string
	Repositories []string
	CanFetch     bool
}

func isPRWorkflow(workflow overload.Workflow) bool {
	return workflow.Kind == "" || workflow.Kind == "pr_review"
}

func mainAgent(workflow overload.Workflow) string {
	if len(workflow.Agents) == 0 {
		return ""
	}
	return workflow.Agents[0]
}

func subAgent(workflow overload.Workflow, slot int) string {
	if slot+1 >= len(workflow.Agents) {
		return ""
	}
	return workflow.Agents[slot+1]
}

func subAgentCount(workflow overload.Workflow) int {
	return max(len(workflow.Agents)-1, 0)
}

func subScope(workflow overload.Workflow, slot int) overload.Scope {
	return workflow.Scopes[subAgent(workflow, slot)]
}

func slotField(slot int, name string) string {
	return fmt.Sprintf("sub_%d_%s", slot, name)
}

func optionalNumber(value int) string {
	if value == 0 {
		return ""
	}
	return strconv.Itoa(value)
}

// scopeChips summarizes a sub-agent's scope as short labels.
func scopeChips(scope overload.Scope) []string {
	var chips []string
	switch scope.Mode {
	case overload.ScopeAlways:
		chips = append(chips, "only matching paths")
	case overload.ScopePlanned:
		chips = append(chips, "planner assigns files")
	}
	chips = append(chips, scope.Paths...)
	if len(scope.Paths) == 0 && scope.Mode != overload.ScopePlanned && (scope.Mode != "" || scope.MaxFindings != 0 || scope.Description != "") {
		chips = append(chips, "every file")
	}
	if scope.MaxFindings > 0 {
		chips = append(chips, fmt.Sprintf("at most %d findings", scope.MaxFindings))
	}
	return chips
}

func subAgents(workflow overload.Workflow) []string {
	if len(workflow.Agents) < 2 {
		return nil
	}
	return workflow.Agents[1:]
}

// workflowDetails summarizes a workflow's type and review options for the
// list page.
func workflowDetails(workflow overload.Workflow) string {
	parts := []string{filterLabel(workflow.Kind)}
	if workflow.MainReviews == overload.MainReviewsUnclaimed {
		parts = append(parts, "main reviews unclaimed files")
	}
	if workflow.PlannerPrompt != "" {
		parts = append(parts, "planner")
	}
	if workflow.VerifierPrompt != "" {
		parts = append(parts, "verifier")
	}
	if n := len(workflow.SkipPaths); n > 0 {
		parts = append(parts, fmt.Sprintf("%d skip paths", n))
	}
	if workflow.MaxFileReviews > 0 {
		parts = append(parts, fmt.Sprintf("limit %d reviews", workflow.MaxFileReviews))
	}
	if workflow.MaxFindings > 0 {
		parts = append(parts, fmt.Sprintf("posts up to %d findings", workflow.MaxFindings))
	}
	return strings.Join(parts, " · ")
}

// findingCount counts a run's findings, noting those the verifier dropped.
func findingCount(findings []overload.Finding) string {
	dropped := 0
	for _, finding := range findings {
		if finding.DropReason != "" {
			dropped++
		}
	}
	if dropped == 0 {
		return strconv.Itoa(len(findings))
	}
	return fmt.Sprintf("%d · %d dropped", len(findings)-dropped, dropped)
}

// filterLabel names a list filter value for people.
func filterLabel(value string) string {
	switch value {
	case "pr_review":
		return "PR review"
	case "scheduled_prompt":
		return "Scheduled prompt"
	case "local":
		return "Local"
	case "hosted":
		return "Hosted"
	default:
		return value
	}
}

// StarterPrompt is built-in text a new agent can start from.
type StarterPrompt struct {
	Name, Label string
}

// promptExcerpt shortens instructions for a table cell.
func promptExcerpt(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= 140 {
		return text
	}
	cut := 140
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

func onOff(text string) string {
	if strings.TrimSpace(text) == "" {
		return "(off)"
	}
	return "(on)"
}

// inWorkflow reports whether the workflow already uses agent, so it stays
// selectable in every slot (for reordering) even when disabled.
func inWorkflow(workflow overload.Workflow, agent string) bool {
	return slices.Contains(workflow.Agents, agent)
}
