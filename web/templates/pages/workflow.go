package pages

import (
	"fmt"
	"strconv"
	"strings"

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

// subAgentSummary describes a workflow's sub-agents for the list page.
func subAgentSummary(workflow overload.Workflow) string {
	if len(workflow.Agents) < 2 {
		return "None"
	}
	var parts []string
	for _, name := range workflow.Agents[1:] {
		if paths := workflow.Scopes[name].Paths; len(paths) > 0 {
			name += " (" + strings.Join(paths, ", ") + ")"
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, "; ")
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
