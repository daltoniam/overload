package httpapi

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
)

// workflowFromForm reads the workflow editor: a main agent, up to
// MaxSubAgents sub-agent slots (sub_N_agent, sub_N_mode, sub_N_paths,
// sub_N_description, sub_N_max_findings) and, for PR reviews, the routing
// settings. Empty slots are ignored. Values that cannot be parsed are
// reported; everything else is left to Workflow.Validate.
func workflowFromForm(values url.Values) (overload.Workflow, error) {
	workflow := overload.Workflow{Name: strings.TrimSpace(values.Get("name")), Kind: values.Get("kind"), Enabled: values.Get("enabled") == "true"}
	if main := values.Get("main_agent"); main != "" {
		workflow.Agents = append(workflow.Agents, main)
	} else {
		for _, name := range values["agents"] {
			if name != "" {
				workflow.Agents = append(workflow.Agents, name)
			}
		}
	}
	pr := workflow.Kind == "pr_review"
	for slot := range overload.MaxSubAgents {
		prefix := fmt.Sprintf("sub_%d_", slot)
		name := values.Get(prefix + "agent")
		if name == "" {
			continue
		}
		workflow.Agents = append(workflow.Agents, name)
		if !pr {
			continue
		}
		scope := overload.Scope{Paths: lines(values.Get(prefix + "paths")), Description: strings.TrimSpace(values.Get(prefix + "description"))}
		if mode := values.Get(prefix + "mode"); mode != overload.ScopeGlobs {
			scope.Mode = mode
		}
		if text := strings.TrimSpace(values.Get(prefix + "max_findings")); text != "" {
			limit, err := strconv.Atoi(text)
			if err != nil {
				return workflow, fmt.Errorf("max findings for %s must be a number", name)
			}
			scope.MaxFindings = limit
		}
		if len(scope.Paths) > 0 || scope.Mode != "" || scope.Description != "" || scope.MaxFindings != 0 {
			if workflow.Scopes == nil {
				workflow.Scopes = map[string]overload.Scope{}
			}
			if _, duplicate := workflow.Scopes[name]; duplicate {
				return workflow, fmt.Errorf("agent %s is listed more than once", name)
			}
			workflow.Scopes[name] = scope
		}
	}
	if !pr {
		return workflow, nil
	}
	workflow.SkipPaths = lines(values.Get("skip_paths"))
	if mode := values.Get("main_reviews"); mode != overload.MainReviewsAll {
		workflow.MainReviews = mode
	}
	workflow.PlannerPrompt, workflow.VerifierPrompt = promptText(values.Get("planner_prompt")), promptText(values.Get("verifier_prompt"))
	if text := strings.TrimSpace(values.Get("max_file_reviews")); text != "" {
		limit, err := strconv.Atoi(text)
		if err != nil {
			return workflow, fmt.Errorf("the file review limit must be a number")
		}
		workflow.MaxFileReviews = limit
	}
	return workflow, nil
}

func lines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// promptText normalizes instructions typed into a form: browsers send CRLF
// line endings, and whitespace-only text means no prompt.
func promptText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return text
}
