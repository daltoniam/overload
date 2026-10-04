package overload

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type RunStatus string

const (
	RunQueued     RunStatus = "queued"
	RunRunning    RunStatus = "running"
	RunCompleted  RunStatus = "completed"
	RunFailed     RunStatus = "failed"
	RunSuperseded RunStatus = "superseded"
)

type Run struct {
	ID           int64
	RepositoryID int64
	PRNumber     int
	HeadSHA      string
	BaseSHA      string
	Trigger      string
	Kind         string
	Status       RunStatus
	Mode         string
	DryRun       bool
	RiverJobID   int64
	// InstallationID is the GitHub App installation pinned when the run
	// was queued; 0 means the configured token.
	InstallationID int64
	CreatedAt      time.Time
	Summary        string
	ErrorCode      string
	ErrorMessage   string
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

type Finding struct {
	ID         int64   `json:"id,omitempty"`
	RunID      int64   `json:"run_id,omitempty"`
	Path       string  `json:"path"`
	Line       int     `json:"line"`
	StartLine  int     `json:"start_line,omitempty"`
	Side       string  `json:"side"`
	Severity   string  `json:"severity"`
	Category   string  `json:"category"`
	Title      string  `json:"title"`
	Body       string  `json:"body"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
}

type Repository struct {
	ID             int64
	InstallationID int64
	FullName       string
	Enabled        bool
	DryRun         bool
}

type ModelProfile struct {
	Name            string
	Provider        string
	ConnectionKind  string
	BaseURL         string
	Model           string
	APIKeyEnv       string
	Headers         map[string]string `json:"-"`
	Concurrency     int
	ReasoningParam  string
	ReasoningEffort string
	MaxOutputTokens int
}

type ReviewAgent struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
}

type ReviewSettings struct {
	Agents          []ReviewAgent `json:"agents"`
	Name            string        `json:"name"`
	Provider        string        `json:"provider"`
	ConnectionKind  string        `json:"connection_kind,omitempty"`
	BaseURL         string        `json:"base_url"`
	Model           string        `json:"model"`
	APIKeyEnv       string        `json:"api_key_env"`
	PromptProfile   string        `json:"prompt_profile"`
	IsDefault       bool          `json:"is_default"`
	Concurrency     int           `json:"concurrency,omitempty"`
	ReasoningParam  string        `json:"reasoning_param,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
}

const MaxReviewConcurrency = 32

// Reasoning parameter styles a model connection can use. ReasoningAuto keeps
// the defaults: local models get the chat-template switch at xhigh, hosted
// models get nothing.
const (
	ReasoningAuto         = ""
	ReasoningNone         = "none"
	ReasoningChatTemplate = "chat_template"
	ReasoningEffortField  = "reasoning_effort"
)

var reasoningEfforts = map[string]bool{"none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}

func ValidateReasoning(param, effort string, maxOutputTokens int) error {
	switch param {
	case ReasoningAuto, ReasoningNone:
		if effort != "" {
			return errors.New("reasoning effort needs a reasoning parameter style")
		}
	case ReasoningChatTemplate, ReasoningEffortField:
		if !reasoningEfforts[effort] {
			return errors.New("reasoning effort must be one of none, minimal, low, medium, high, xhigh, max")
		}
	default:
		return errors.New("reasoning parameter must be empty, none, chat_template or reasoning_effort")
	}
	if maxOutputTokens != 0 && (maxOutputTokens < 256 || maxOutputTokens > 262144) {
		return errors.New("max output tokens must be between 256 and 262144")
	}
	return nil
}

// validateAPIKeyEnv rejects variables that hold overload's own secrets. The
// named variable's value is sent as a bearer token to the model URL, which
// anyone who can edit models controls.
func validateAPIKeyEnv(name string) error {
	if name == "" {
		return nil
	}
	if !envName.MatchString(name) {
		return errors.New("invalid API key environment variable name")
	}
	upper := strings.ToUpper(name)
	allowedOverload := strings.HasPrefix(upper, "OVERLOAD_MODEL_") || upper == "OVERLOAD_DEFAULT_MODEL_API_KEY"
	if upper == "DATABASE_URL" || strings.HasPrefix(upper, "PG") || strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "GH_") || (strings.HasPrefix(upper, "OVERLOAD_") && !allowedOverload) {
		return fmt.Errorf("%s holds an overload or GitHub secret and cannot be used as a model API key", name)
	}
	return nil
}

var settingName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (settings ReviewSettings) Validate() error {
	if !settingName.MatchString(settings.Name) || settings.Provider != "openaicompat" || strings.TrimSpace(settings.Model) == "" || len(settings.Model) > 200 || strings.TrimSpace(settings.Model) != settings.Model {
		return errors.New("invalid model name or provider")
	}
	if settings.PromptProfile != "context" && settings.PromptProfile != "switchboard-go" {
		return errors.New("unsupported prompt profile")
	}
	if settings.ConnectionKind != "" && settings.ConnectionKind != "local" && settings.ConnectionKind != "hosted" {
		return errors.New("invalid model connection type")
	}
	if err := validateAPIKeyEnv(settings.APIKeyEnv); err != nil {
		return err
	}
	if settings.ConnectionKind == "hosted" && settings.APIKeyEnv == "" {
		return errors.New("hosted models require an API key environment variable")
	}
	if settings.Concurrency < 0 || settings.Concurrency > MaxReviewConcurrency {
		return fmt.Errorf("concurrency must be between 1 and %d", MaxReviewConcurrency)
	}
	if err := ValidateReasoning(settings.ReasoningParam, settings.ReasoningEffort, settings.MaxOutputTokens); err != nil {
		return err
	}
	if len(settings.Agents) > 4 {
		return errors.New("at most four review agents are supported")
	}
	seen := make(map[string]bool)
	for _, agent := range settings.Agents {
		if !settingName.MatchString(agent.Name) || seen[agent.Name] || strings.TrimSpace(agent.Instructions) == "" || len(agent.Instructions) > 4000 {
			return errors.New("invalid review agent")
		}
		seen[agent.Name] = true
	}
	parsed, err := url.Parse(settings.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || len(settings.BaseURL) > 2048 || strings.ContainsAny(settings.BaseURL, "\r\n") {
		return errors.New("invalid model base URL")
	}
	return nil
}

func (settings ReviewSettings) Profile() ModelProfile {
	return ModelProfile{Name: settings.Name, Provider: settings.Provider, ConnectionKind: settings.ConnectionKind, BaseURL: settings.BaseURL, Model: settings.Model, APIKeyEnv: settings.APIKeyEnv, Concurrency: settings.Concurrency, ReasoningParam: settings.ReasoningParam, ReasoningEffort: settings.ReasoningEffort, MaxOutputTokens: settings.MaxOutputTokens}
}

type ReviewSpec struct {
	Repository Repository       `json:"repository"`
	PRNumber   int              `json:"pr_number"`
	Diff       string           `json:"diff"`
	Workflow   ResolvedWorkflow `json:"workflow"`
}

type ReviewResult struct {
	Findings []Finding      `json:"findings"`
	Summary  string         `json:"summary"`
	Metrics  map[string]any `json:"metrics,omitempty"`
	Error    string         `json:"error,omitempty"`
}
