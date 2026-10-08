package overload

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ToolServer is an MCP server whose tools scheduled agents can call, such
// as a self-hosted Switchboard or hosted Switchboard. TokenEnv names the
// environment variable holding its bearer token; the token itself is never
// stored.
type ToolServer struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	TokenEnv    string `json:"token_env,omitempty"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// ToolTokenEnvPrefix is the required prefix of a tool server's token
// variable. The token is sent to the server's URL, which anyone who can edit
// tool servers controls, so only variables meant for tool servers qualify.
const ToolTokenEnvPrefix = "OVERLOAD_TOOL_"

// MaxAgentTools caps the tool servers one agent can use.
const MaxAgentTools = 8

// Limits for scheduled agents that call tools. A step is one model call;
// the agent stops after MaxSteps steps or TimeoutMinutes, whichever comes
// first.
const (
	DefaultMaxSteps       = 40
	MaxStepsLimit         = 200
	DefaultTimeoutMinutes = 30
	MaxTimeoutMinutes     = 240
)

func (server ToolServer) Validate() error {
	if !settingName.MatchString(server.Name) {
		return errors.New("tool server name must be letters, digits, - or _")
	}
	parsed, err := url.Parse(server.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || len(server.URL) > 2048 || strings.ContainsAny(server.URL, "\r\n ") {
		return errors.New("tool server URL must be an http or https URL without credentials")
	}
	if server.TokenEnv != "" {
		if !envName.MatchString(server.TokenEnv) || !strings.HasPrefix(server.TokenEnv, ToolTokenEnvPrefix) || server.TokenEnv == ToolTokenEnvPrefix {
			return fmt.Errorf("token variable must start with %s", ToolTokenEnvPrefix)
		}
	}
	if len(server.Description) > 500 || strings.ContainsRune(server.Description, 0) {
		return errors.New("tool server description must be at most 500 characters")
	}
	return nil
}

func validateAgentTools(kind string, tools []string) error {
	if len(tools) == 0 {
		return nil
	}
	if kind != "scheduled_prompt" {
		return errors.New("only scheduled agents can use tools: PR reviews read untrusted pull request text")
	}
	if len(tools) > MaxAgentTools {
		return fmt.Errorf("an agent can use at most %d tool servers", MaxAgentTools)
	}
	seen := map[string]bool{}
	for _, name := range tools {
		if !settingName.MatchString(name) || seen[name] {
			return errors.New("invalid or repeated tool server")
		}
		seen[name] = true
	}
	return nil
}

func validateRunLimits(kind string, maxSteps, timeoutMinutes int) error {
	if kind != "scheduled_prompt" && (maxSteps != 0 || timeoutMinutes != 0) {
		return errors.New("only scheduled workflows have step and time limits")
	}
	if maxSteps < 0 || maxSteps > MaxStepsLimit {
		return fmt.Errorf("max_steps must be between 1 and %d (0 means %d)", MaxStepsLimit, DefaultMaxSteps)
	}
	if timeoutMinutes < 0 || timeoutMinutes > MaxTimeoutMinutes {
		return fmt.Errorf("timeout_minutes must be between 1 and %d (0 means %d)", MaxTimeoutMinutes, DefaultTimeoutMinutes)
	}
	return nil
}

// RunLimits returns a scheduled workflow's step and time limits with
// defaults applied.
func (workflow ResolvedWorkflow) RunLimits() (maxSteps, timeoutMinutes int) {
	maxSteps, timeoutMinutes = workflow.MaxSteps, workflow.TimeoutMinutes
	if maxSteps == 0 {
		maxSteps = DefaultMaxSteps
	}
	if timeoutMinutes == 0 {
		timeoutMinutes = DefaultTimeoutMinutes
	}
	return maxSteps, timeoutMinutes
}
