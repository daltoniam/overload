package overload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxPromptLength bounds an agent, planner or verifier prompt.
const MaxPromptLength = 16000

// PromptTemplate is one pinned version of an agent's, planner's or
// verifier's instructions. Agents and workflows own their prompts; every
// change to the text is stored as a new revision so runs record exactly
// what they ran with.
type PromptTemplate struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Revision int    `json:"revision"`
	Body     string `json:"body"`
	SHA256   string `json:"sha256"`
}

func validatePromptText(label, text string, required bool) error {
	switch {
	case strings.TrimSpace(text) == "" && required:
		return fmt.Errorf("%s is required", label)
	case len(text) > MaxPromptLength:
		return fmt.Errorf("%s must be at most %d bytes", label, MaxPromptLength)
	case !utf8.ValidString(text) || strings.ContainsRune(text, 0):
		return fmt.Errorf("%s must be valid text", label)
	}
	return nil
}

// Prompt kinds: an agent's instructions, and a workflow's optional planner
// and verifier instructions, which run on the main agent's model.
const (
	PromptEntry  = "entry"
	PromptPlan   = "plan"
	PromptVerify = "verify"
)

func PromptDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// AgentDefinition is a model and the instructions it reviews with.
// PromptRevision counts saved changes to Prompt. When saving, a non-zero
// value is the version the edit started from.
type AgentDefinition struct {
	Name           string `json:"name"`
	Kind           string `json:"kind,omitempty"`
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Enabled        bool   `json:"enabled"`
	PromptRevision int    `json:"prompt_revision,omitempty"`
	// Tools names the tool servers a scheduled agent can call.
	Tools []string `json:"tools,omitempty"`
}

func (agent AgentDefinition) Validate() error {
	if !settingName.MatchString(agent.Name) || !settingName.MatchString(agent.Model) {
		return errors.New("invalid agent definition")
	}
	if err := validatePromptText("the agent prompt", agent.Prompt, true); err != nil {
		return err
	}
	if agent.Kind != "" && agent.Kind != "pr_review" && agent.Kind != "scheduled_prompt" {
		return errors.New("invalid agent type")
	}
	kind := agent.Kind
	if kind == "" {
		kind = "pr_review"
	}
	return validateAgentTools(kind, agent.Tools)
}

// MaxSubAgents caps sub-agents per workflow. A workflow's first agent is its
// main agent and the rest are sub-agents.
const MaxSubAgents = 8

// Workflow is a saved workflow. Revision counts saved changes; when saving, a
// non-zero value is the revision the edit started from.
type Workflow struct {
	Name           string           `json:"name"`
	Kind           string           `json:"kind"`
	Revision       int              `json:"revision"`
	Agents         []string         `json:"agents"`
	Enabled        bool             `json:"enabled"`
	Scopes         map[string]Scope `json:"scopes,omitempty"`
	SkipPaths      []string         `json:"skip_paths,omitempty"`
	MainReviews    string           `json:"main_reviews,omitempty"`
	MaxFileReviews int              `json:"max_file_reviews,omitempty"`
	// MaxFindings is the most findings a review posts; 0 means
	// DefaultMaxFindings.
	MaxFindings int `json:"max_findings,omitempty"`
	// PlannerPrompt and VerifierPrompt are the planner's and verifier's
	// instructions; empty means no planner or verifier.
	PlannerPrompt  string `json:"planner_prompt,omitempty"`
	VerifierPrompt string `json:"verifier_prompt,omitempty"`
	// MaxSteps and TimeoutMinutes limit each agent of a scheduled workflow;
	// 0 means DefaultMaxSteps and DefaultTimeoutMinutes.
	MaxSteps       int `json:"max_steps,omitempty"`
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
	// ReviewDecision and BlockSeverity decide whether a PR review only
	// comments, requests changes or approves; see ReviewEvent.
	ReviewDecision string `json:"review_decision,omitempty"`
	BlockSeverity  string `json:"block_severity,omitempty"`
}

func (workflow Workflow) Validate() error {
	if !settingName.MatchString(workflow.Name) || (workflow.Kind != "pr_review" && workflow.Kind != "scheduled_prompt") || len(workflow.Agents) == 0 || len(workflow.Agents) > 1+MaxSubAgents {
		return errors.New("invalid workflow")
	}
	seen := make(map[string]bool)
	for _, name := range workflow.Agents {
		if !settingName.MatchString(name) || seen[name] {
			return errors.New("invalid workflow agents")
		}
		seen[name] = true
	}
	for name := range workflow.Scopes {
		if !seen[name] || name == workflow.Agents[0] {
			return fmt.Errorf("scope for %q: scopes apply to the workflow's sub-agents", name)
		}
	}
	if err := validatePromptText("the planner prompt", workflow.PlannerPrompt, false); err != nil {
		return err
	}
	if err := validatePromptText("the verifier prompt", workflow.VerifierPrompt, false); err != nil {
		return err
	}
	if err := validateRunLimits(workflow.Kind, workflow.MaxSteps, workflow.TimeoutMinutes); err != nil {
		return err
	}
	if err := validateReviewDecision(workflow.Kind, workflow.ReviewDecision, workflow.BlockSeverity); err != nil {
		return err
	}
	return validateRouting(routingSettings{kind: workflow.Kind, skipPaths: workflow.SkipPaths, mainReviews: workflow.MainReviews, maxFileReviews: workflow.MaxFileReviews, maxFindings: workflow.MaxFindings, scopes: workflow.Scopes, planner: strings.TrimSpace(workflow.PlannerPrompt) != "", verifier: strings.TrimSpace(workflow.VerifierPrompt) != ""})
}

type TriggerBinding struct {
	ID         int64  `json:"id"`
	Source     string `json:"source"`
	Event      string `json:"event"`
	Action     string `json:"action"`
	Repository string `json:"repository"`
	Workflow   string `json:"workflow"`
	Enabled    bool   `json:"enabled"`
}

func (binding TriggerBinding) Validate() error {
	if !settingName.MatchString(binding.Source) || !settingName.MatchString(binding.Event) || !settingName.MatchString(binding.Action) || !settingName.MatchString(binding.Workflow) || len(binding.Repository) > 200 {
		return errors.New("invalid trigger binding")
	}
	if binding.Repository != "" && (strings.Count(binding.Repository, "/") != 1 || strings.ContainsAny(binding.Repository, " \r\n")) {
		return errors.New("invalid repository")
	}
	return nil
}

type Schedule struct {
	Name      string          `json:"name"`
	Workflow  string          `json:"workflow"`
	Cron      string          `json:"cron"`
	Timezone  string          `json:"timezone"`
	Input     json.RawMessage `json:"input"`
	NextRunAt time.Time       `json:"next_run_at"`
	Enabled   bool            `json:"enabled"`
}

type ResolvedAgent struct {
	Name        string         `json:"name"`
	Model       ModelProfile   `json:"model"`
	EntryPrompt PromptTemplate `json:"entry_prompt"`
	// LegacyFocus is the review focus prompt of snapshots pinned before
	// agents had a single prompt; new snapshots never set it.
	LegacyFocus PromptTemplate `json:"review_prompt,omitzero"`
	Scope       Scope          `json:"scope"`
	// Tools are the tool servers a scheduled agent can call, as configured
	// when the run was queued.
	Tools []ToolServer `json:"tools,omitempty"`
}

type ResolvedWorkflow struct {
	Version        int             `json:"version,omitempty"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	Revision       int             `json:"revision"`
	Agents         []ResolvedAgent `json:"agents"`
	SkipPaths      []string        `json:"skip_paths,omitempty"`
	MainReviews    string          `json:"main_reviews,omitempty"`
	MaxFileReviews int             `json:"max_file_reviews,omitempty"`
	MaxFindings    int             `json:"max_findings,omitempty"`
	PlannerPrompt  *PromptTemplate `json:"planner_prompt,omitempty"`
	VerifierPrompt *PromptTemplate `json:"verifier_prompt,omitempty"`
	MaxSteps       int             `json:"max_steps,omitempty"`
	TimeoutMinutes int             `json:"timeout_minutes,omitempty"`
	ReviewDecision string          `json:"review_decision,omitempty"`
	BlockSeverity  string          `json:"block_severity,omitempty"`
}

// Verify checks a pinned workflow once before it runs: a known kind, a main
// agent and at most MaxSubAgents sub-agents with distinct names, a usable
// model for each, and prompt bodies that still match the digests recorded
// when the run was queued.
func (workflow ResolvedWorkflow) Verify() error {
	if workflow.Kind != "pr_review" && workflow.Kind != "scheduled_prompt" {
		return fmt.Errorf("unsupported workflow kind %q", workflow.Kind)
	}
	if workflow.Version != 0 && workflow.Version != 2 && workflow.Version != SnapshotVersion {
		return fmt.Errorf("workflow snapshot version %d is not supported", workflow.Version)
	}
	scopes := map[string]Scope{}
	for index, agent := range workflow.Agents {
		if agent.Scope.empty() {
			continue
		}
		if index == 0 {
			return errors.New("the main agent cannot have a scope")
		}
		scopes[agent.Name] = agent.Scope
	}
	if err := validateRouting(routingSettings{kind: workflow.Kind, skipPaths: workflow.SkipPaths, mainReviews: workflow.MainReviews, maxFileReviews: workflow.MaxFileReviews, maxFindings: workflow.MaxFindings, scopes: scopes, planner: workflow.PlannerPrompt != nil, verifier: workflow.VerifierPrompt != nil}); err != nil {
		return err
	}
	if err := validateRunLimits(workflow.Kind, workflow.MaxSteps, workflow.TimeoutMinutes); err != nil {
		return err
	}
	if err := validateReviewDecision(workflow.Kind, workflow.ReviewDecision, workflow.BlockSeverity); err != nil {
		return err
	}
	if workflow.Version < 3 && (workflow.PlannerPrompt != nil || workflow.VerifierPrompt != nil) {
		return errors.New("planner and verifier prompts need workflow snapshot version 3")
	}
	for kind, prompt := range map[string]*PromptTemplate{PromptPlan: workflow.PlannerPrompt, PromptVerify: workflow.VerifierPrompt} {
		if prompt != nil && (prompt.Kind != kind || PromptDigest(prompt.Body) != prompt.SHA256) {
			return fmt.Errorf("workflow %s prompt does not match its pinned revision", kind)
		}
	}
	if len(workflow.Agents) == 0 || len(workflow.Agents) > 1+MaxSubAgents {
		return fmt.Errorf("a workflow needs a main agent and at most %d sub-agents", MaxSubAgents)
	}
	seen := make(map[string]bool)
	for _, agent := range workflow.Agents {
		if seen[agent.Name] {
			return fmt.Errorf("agent %q appears more than once", agent.Name)
		}
		seen[agent.Name] = true
		if agent.Name == "" || agent.Model.Provider != "openaicompat" || agent.Model.Model == "" || agent.Model.BaseURL == "" {
			return fmt.Errorf("agent %q has no usable model", agent.Name)
		}
		if agent.EntryPrompt.Kind != "entry" || PromptDigest(agent.EntryPrompt.Body) != agent.EntryPrompt.SHA256 {
			return fmt.Errorf("agent %q entry prompt does not match its pinned revision", agent.Name)
		}
		if agent.LegacyFocus.Kind != "" && (agent.LegacyFocus.Kind != "review" || PromptDigest(agent.LegacyFocus.Body) != agent.LegacyFocus.SHA256) {
			return fmt.Errorf("agent %q review prompt does not match its pinned revision", agent.Name)
		}
		names := make([]string, 0, len(agent.Tools))
		for _, server := range agent.Tools {
			if err := server.Validate(); err != nil {
				return fmt.Errorf("agent %q tool server %q: %w", agent.Name, server.Name, err)
			}
			names = append(names, server.Name)
		}
		if err := validateAgentTools(workflow.Kind, names); err != nil {
			return fmt.Errorf("agent %q: %w", agent.Name, err)
		}
	}
	return nil
}

// FindingFingerprint identifies a finding across runs of the same pull
// request. It ignores the line number so pushes that only shift code do not
// produce duplicate comments.
func FindingFingerprint(repository string, pr int, finding Finding) string {
	normalize := func(value string) string { return strings.Join(strings.Fields(strings.ToLower(value)), " ") }
	return PromptDigest(strings.Join([]string{strings.ToLower(repository), strconv.Itoa(pr), finding.Path, normalize(finding.Category), normalize(finding.Title), normalize(finding.Evidence)}, "\x00"))
}

// TruncateUTF8 shortens value to at most limit bytes without splitting a
// character, so the result can be stored in Postgres text columns.
func TruncateUTF8(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
