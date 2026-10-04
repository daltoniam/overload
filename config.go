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

type PromptTemplate struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Revision int    `json:"revision"`
	Body     string `json:"body"`
	SHA256   string `json:"sha256"`
}

func (prompt PromptTemplate) Validate() error {
	if !settingName.MatchString(prompt.Name) || !validPromptKind(prompt.Kind) || strings.TrimSpace(prompt.Body) == "" || len(prompt.Body) > 16000 {
		return errors.New("invalid prompt template")
	}
	return nil
}

func validPromptKind(kind string) bool {
	return kind == "entry" || kind == "review"
}

func PromptDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

type AgentDefinition struct {
	Name           string `json:"name"`
	Kind           string `json:"kind,omitempty"`
	Model          string `json:"model"`
	EntryPrompt    string `json:"entry_prompt"`
	ReviewPrompt   string `json:"review_prompt,omitempty"`
	Enabled        bool   `json:"enabled"`
	EntryRevision  int    `json:"-"`
	ReviewRevision int    `json:"-"`
}

func (agent AgentDefinition) Validate() error {
	if !settingName.MatchString(agent.Name) || !settingName.MatchString(agent.Model) || !settingName.MatchString(agent.EntryPrompt) {
		return errors.New("invalid agent definition")
	}
	if agent.Kind != "" && agent.Kind != "pr_review" && agent.Kind != "scheduled_prompt" {
		return errors.New("invalid agent type")
	}
	if agent.ReviewPrompt != "" && !settingName.MatchString(agent.ReviewPrompt) {
		return errors.New("invalid review prompt")
	}
	return nil
}

// MaxSubAgents caps sub-agents per workflow. A workflow's first agent is its
// main agent and the rest are sub-agents.
const MaxSubAgents = 8

type Workflow struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Revision int      `json:"revision"`
	Agents   []string `json:"agents"`
	Enabled  bool     `json:"enabled"`
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
	return nil
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
	Name         string         `json:"name"`
	Model        ModelProfile   `json:"model"`
	EntryPrompt  PromptTemplate `json:"entry_prompt"`
	ReviewPrompt PromptTemplate `json:"review_prompt"`
}

type ResolvedWorkflow struct {
	Name     string          `json:"name"`
	Kind     string          `json:"kind"`
	Revision int             `json:"revision"`
	Agents   []ResolvedAgent `json:"agents"`
}

// Verify checks a pinned workflow once before it runs: a known kind, a main
// agent and at most MaxSubAgents sub-agents with distinct names, a usable
// model for each, and prompt bodies that still match the digests recorded
// when the run was queued.
func (workflow ResolvedWorkflow) Verify() error {
	if workflow.Kind != "pr_review" && workflow.Kind != "scheduled_prompt" {
		return fmt.Errorf("unsupported workflow kind %q", workflow.Kind)
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
		if agent.ReviewPrompt.Kind != "" && (agent.ReviewPrompt.Kind != "review" || PromptDigest(agent.ReviewPrompt.Body) != agent.ReviewPrompt.SHA256) {
			return fmt.Errorf("agent %q review prompt does not match its pinned revision", agent.Name)
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
