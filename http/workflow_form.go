package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
)

// workflowFromForm reads the workflow editor: a main agent, up to
// MaxSubAgents sub-agent slots (sub_N_agent, sub_N_mode, sub_N_paths,
// sub_N_description, sub_N_max_findings) and, for PR reviews, the routing
// settings. Empty slots are ignored. Every field is read even when a number
// does not parse, so the form can be shown again intact; the first parse
// error is returned. Everything else is left to Workflow.Validate.
func workflowFromForm(values url.Values) (overload.Workflow, error) {
	var parseErr error
	number := func(text, message string) int {
		text = strings.TrimSpace(text)
		if text == "" {
			return 0
		}
		value, err := strconv.Atoi(text)
		if err != nil && parseErr == nil {
			parseErr = errors.New(message)
		}
		return value
	}
	workflow := overload.Workflow{Name: values.Get("name"), Kind: values.Get("kind"), Enabled: values.Get("enabled") == "true", Revision: number(values.Get("revision"), "invalid workflow revision")}
	if main := values.Get("main_agent"); main != "" {
		workflow.Agents = append(workflow.Agents, main)
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
		if workflow.Scopes == nil {
			workflow.Scopes = map[string]overload.Scope{}
		}
		workflow.Scopes[name] = overload.Scope{
			Paths:       strings.Split(values.Get(prefix+"paths"), "\n"),
			Mode:        values.Get(prefix + "mode"),
			Description: values.Get(prefix + "description"),
			MaxFindings: number(values.Get(prefix+"max_findings"), "max findings for "+name+" must be a number"),
		}
	}
	if pr {
		workflow.SkipPaths = strings.Split(values.Get("skip_paths"), "\n")
		workflow.MainReviews = values.Get("main_reviews")
		workflow.PlannerPrompt, workflow.VerifierPrompt = values.Get("planner_prompt"), values.Get("verifier_prompt")
		workflow.MaxFileReviews = number(values.Get("max_file_reviews"), "the file review limit must be a number")
		workflow.MaxFindings = number(values.Get("max_findings"), "the finding limit must be a number")
		workflow.MaxFileChars = number(values.Get("max_file_chars"), "the largest file size must be a number")
		if err := policyFromForm(values, &workflow.ReviewPolicy); err != nil && parseErr == nil {
			parseErr = err
		}
		workflow.ReviewDecision = values.Get("review_decision")
		if workflow.ReviewDecision != "" && workflow.ReviewDecision != overload.ReviewDecisionComment {
			workflow.BlockSeverity = values.Get("block_severity")
		}
	} else {
		workflow.MaxSteps = number(values.Get("max_steps"), "the step limit must be a number")
		workflow.TimeoutMinutes = number(values.Get("timeout_minutes"), "the time limit must be a number")
	}
	workflow.Normalize()
	return workflow, parseErr
}

// policyFromForm reads the review policy JSON and the cost cap field.
func policyFromForm(values url.Values, policy *overload.ReviewPolicy) error {
	var parseErr error
	if text := strings.TrimSpace(values.Get("review_policy")); text != "" {
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(policy); err != nil {
			parseErr = fmt.Errorf("the review policy is not valid JSON: %w", err)
		}
	}
	if text := strings.TrimSpace(values.Get("max_run_cost_usd")); text != "" {
		cost, err := strconv.ParseFloat(text, 64)
		if err != nil && parseErr == nil {
			parseErr = errors.New("the most a review may spend must be a number")
		}
		policy.MaxRunCostUSD = cost
	}
	return parseErr
}
