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
	// ForgeID is the GitLab or Forgejo connection the run's repository is
	// on; 0 means GitHub.
	ForgeID      int64
	CreatedAt    time.Time
	Summary      string
	ErrorCode    string
	ErrorMessage string
	StartedAt    *time.Time
	FinishedAt   *time.Time
	// Cost is what the run's model calls cost; nil when not recorded.
	Cost *Spend
}

type Finding struct {
	ID         int64    `json:"id,omitempty"`
	RunID      int64    `json:"run_id,omitempty"`
	Path       string   `json:"path"`
	Line       int      `json:"line"`
	StartLine  int      `json:"start_line,omitempty"`
	Side       string   `json:"side"`
	Severity   string   `json:"severity"`
	Category   string   `json:"category"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Confidence float64  `json:"confidence"`
	Evidence   string   `json:"evidence"`
	Agents     []string `json:"agents,omitempty"`
	// DropReason is set on findings the verifier dropped. They are stored
	// on the run but never posted.
	DropReason string `json:"drop_reason,omitempty"`
	// CommentID is the GitHub review comment the finding was posted as,
	// when known.
	CommentID int64 `json:"comment_id,omitempty"`
}

type Repository struct {
	ID             int64
	InstallationID int64
	// ForgeID is the GitLab or Forgejo connection the repository is on;
	// 0 means GitHub. Forge is that connection's name and ForgeKind its
	// kind.
	ForgeID   int64  `json:",omitempty"`
	Forge     string `json:",omitempty"`
	ForgeKind string `json:",omitempty"`
	FullName  string
	Enabled   bool
	DryRun    bool
}

// Ref names the repository unambiguously across hosts: owner/name on
// GitHub, connection:path on a GitLab or Forgejo connection. Repository
// paths never contain a colon.
func (repo Repository) Ref() string {
	if repo.Forge == "" {
		return repo.FullName
	}
	return repo.Forge + ":" + repo.FullName
}

// Host is the kind of git host the repository is on.
func (repo Repository) Host() string {
	if repo.ForgeKind == "" {
		return ForgeGitHub
	}
	return repo.ForgeKind
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
	// API is the request style: "" for Chat Completions, APIResponses for
	// OpenAI's Responses API.
	API string `json:",omitempty"`
	// Pricing is the model's price list; nil means calls are unpriced.
	Pricing *Pricing `json:",omitempty"`
}

type ReviewSettings struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	ConnectionKind  string `json:"connection_kind,omitempty"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	APIKeyEnv       string `json:"api_key_env"`
	PromptProfile   string `json:"prompt_profile"`
	IsDefault       bool   `json:"is_default"`
	Concurrency     int    `json:"concurrency,omitempty"`
	ReasoningParam  string `json:"reasoning_param,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	API             string `json:"api,omitempty"`
	// Prices in US dollars per million tokens. Input and output are set
	// together; cached input and cache writes default to the input price.
	PriceInput       *float64 `json:"price_input,omitempty"`
	PriceCachedInput *float64 `json:"price_cached_input,omitempty"`
	PriceCacheWrite  *float64 `json:"price_cache_write,omitempty"`
	PriceOutput      *float64 `json:"price_output,omitempty"`
	// Above LongContextTokens prompt tokens in one call, input and output
	// prices are multiplied by the long-context multipliers.
	LongContextTokens           int64   `json:"long_context_tokens,omitempty"`
	LongContextInputMultiplier  float64 `json:"long_context_input_multiplier,omitempty"`
	LongContextOutputMultiplier float64 `json:"long_context_output_multiplier,omitempty"`
}

// Pricing returns the settings' price list, or nil when unpriced.
func (settings ReviewSettings) Pricing() *Pricing {
	if settings.PriceInput == nil || settings.PriceOutput == nil {
		return nil
	}
	pricing := &Pricing{Input: *settings.PriceInput, CachedInput: *settings.PriceInput, CacheWrite: *settings.PriceInput, Output: *settings.PriceOutput, LongContextTokens: settings.LongContextTokens, LongContextInput: settings.LongContextInputMultiplier, LongContextOutput: settings.LongContextOutputMultiplier}
	if settings.PriceCachedInput != nil {
		pricing.CachedInput = *settings.PriceCachedInput
	}
	if settings.PriceCacheWrite != nil {
		pricing.CacheWrite = *settings.PriceCacheWrite
	}
	return pricing
}

// Model providers: any OpenAI-compatible API, or Anthropic's Messages API.
const (
	ProviderOpenAICompat = "openaicompat"
	ProviderAnthropic    = "anthropic"
)

// anthropicEfforts are the effort levels Anthropic's adaptive thinking
// takes (output_config.effort).
var anthropicEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// APIResponses selects OpenAI's Responses API instead of Chat Completions.
// Newer OpenAI reasoning models only accept tools together with a
// reasoning effort there.
const APIResponses = "responses"

const MaxReviewConcurrency = 32

// Finding limits. A model's reply for one file keeps its most severe
// MaxFindingsPerFile findings. A review posts at most the workflow's
// finding limit (DefaultMaxFindings unless set, at most MaxFindingsLimit);
// the rest are kept on the run as dropped.
const (
	MaxFindingsPerFile = 10
	DefaultMaxFindings = 10
	MaxFindingsLimit   = 50
)

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
	for _, prefix := range infrastructureSecretPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return fmt.Errorf("%s looks like an infrastructure credential and cannot be used as a model API key", name)
		}
	}
	for _, part := range []string{"PASSWORD", "PASSWD", "PRIVATE_KEY"} {
		if strings.Contains(upper, part) {
			return fmt.Errorf("%s looks like a password or private key and cannot be used as a model API key", name)
		}
	}
	return nil
}

// infrastructureSecretPrefixes name environment variables that commonly hold
// cloud, cluster or database credentials rather than model API keys.
var infrastructureSecretPrefixes = []string{"AWS_", "AZURE_", "GOOGLE_", "GCP_", "GCLOUD_", "KUBE", "SSH_", "DOCKER_", "POSTGRES", "MYSQL", "REDIS", "VAULT_", "NPM_", "RIVER_"}

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// ValidRepositoryName reports whether name is a GitHub owner/name.
func ValidRepositoryName(name string) bool {
	_, repo, _ := strings.Cut(name, "/")
	return repositoryName.MatchString(name) && repo != "." && repo != ".."
}

var settingName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (settings ReviewSettings) Validate() error {
	if !settingName.MatchString(settings.Name) || (settings.Provider != ProviderOpenAICompat && settings.Provider != ProviderAnthropic) || strings.TrimSpace(settings.Model) == "" || len(settings.Model) > 200 || strings.TrimSpace(settings.Model) != settings.Model {
		return errors.New("invalid model name or provider")
	}
	if settings.Provider == ProviderAnthropic {
		switch {
		case settings.ConnectionKind != "hosted":
			return errors.New("an Anthropic model is a hosted connection")
		case settings.API != "":
			return errors.New("an Anthropic model uses the Messages API; leave the API style empty")
		case settings.ReasoningParam == ReasoningChatTemplate:
			return errors.New("an Anthropic model takes an effort (reasoning_effort), not the chat template switch")
		case settings.ReasoningParam == ReasoningEffortField && !anthropicEfforts[settings.ReasoningEffort]:
			return errors.New("the Anthropic effort must be low, medium, high, xhigh or max")
		}
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
	switch settings.API {
	case "":
	case APIResponses:
		if settings.ReasoningParam == ReasoningChatTemplate {
			return errors.New("the Responses API takes the reasoning_effort field, not the chat template switch")
		}
	default:
		return errors.New("API must be empty (Chat Completions) or responses")
	}
	if err := validatePricing(settings.PriceInput, settings.PriceCachedInput, settings.PriceCacheWrite, settings.PriceOutput, settings.LongContextTokens, settings.LongContextInputMultiplier, settings.LongContextOutputMultiplier); err != nil {
		return err
	}
	parsed, err := url.Parse(settings.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || len(settings.BaseURL) > 2048 || strings.ContainsAny(settings.BaseURL, "\r\n") {
		return errors.New("invalid model base URL")
	}
	return nil
}

func (settings ReviewSettings) Profile() ModelProfile {
	return ModelProfile{Name: settings.Name, Provider: settings.Provider, ConnectionKind: settings.ConnectionKind, BaseURL: settings.BaseURL, Model: settings.Model, APIKeyEnv: settings.APIKeyEnv, Concurrency: settings.Concurrency, ReasoningParam: settings.ReasoningParam, ReasoningEffort: settings.ReasoningEffort, MaxOutputTokens: settings.MaxOutputTokens, API: settings.API, Pricing: settings.Pricing()}
}

type ReviewSpec struct {
	Repository Repository       `json:"repository"`
	PRNumber   int              `json:"pr_number"`
	Diff       string           `json:"diff"`
	Workflow   ResolvedWorkflow `json:"workflow"`
	// PullRequest is the pull request's title and description, already
	// stripped of links, HTML and mentions; they are untrusted text.
	PullRequest *PullRequestInfo `json:"pull_request,omitempty"`
	// Incremental makes this a re-review of what changed since an earlier
	// review.
	Incremental *Incremental `json:"incremental,omitempty"`
	// Tier is the size tier the workflow's tiers put the pull request in;
	// a lighter tier has already narrowed Workflow to its agents.
	Tier *TierDecision `json:"tier,omitempty"`
}

// Incremental describes a re-review: the files whose diff is the same as
// at the earlier review (they are not reviewed again unless linked to a
// changed file), and the earlier review's open findings.
type Incremental struct {
	SinceRunID int64    `json:"since_run_id"`
	Unchanged  []string `json:"unchanged,omitempty"`
	// Reverted lists files the earlier review's diff changed that the
	// current diff no longer does: the pull request put them back.
	Reverted []string       `json:"reverted,omitempty"`
	Prior    []PriorFinding `json:"prior,omitempty"`
}

// PriorFinding is an open finding from an earlier review of the pull
// request, with replies people left on its thread (untrusted text,
// stripped of links, HTML and mentions).
type PriorFinding struct {
	ID       int64    `json:"id"`
	Path     string   `json:"path"`
	Line     int      `json:"line"`
	Severity string   `json:"severity"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Replies  []string `json:"replies,omitempty"`
	// CommentID is the GitHub review comment the finding was posted as,
	// when known.
	CommentID int64 `json:"comment_id,omitempty"`
}

// PriorVerdict is a re-review's judgment of an earlier finding.
type PriorVerdict struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

// Prior finding statuses.
const (
	PriorResolved     = "resolved"
	PriorStillPresent = "still_present"
)

// PullRequestInfo is what a pull request says about itself.
type PullRequestInfo struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

type ReviewResult struct {
	Findings []Finding `json:"findings"`
	Dropped  []Finding `json:"dropped,omitempty"`
	// Prior judges earlier findings in an incremental re-review.
	Prior   []PriorVerdict `json:"prior,omitempty"`
	Summary string         `json:"summary"`
	Metrics map[string]any `json:"metrics,omitempty"`
	Error   string         `json:"error,omitempty"`
}
