package overload

import (
	"errors"
	"fmt"
	"math"
)

// ReviewPolicy holds a PR workflow's cost and re-review settings. It is
// embedded in stored and pinned workflows, so its fields appear at the top
// level of workflow JSON.
type ReviewPolicy struct {
	// MaxRunCostUSD caps what one review may spend on priced models; 0
	// means no cap. A review that reaches it completes degraded.
	MaxRunCostUSD float64 `json:"max_run_cost_usd,omitempty"`
	// Context adds a shared pull request bundle to every review call; nil
	// sends each file on its own.
	Context *ContextSettings `json:"context,omitempty"`
}

// ContextSettings shape the pull request context bundle: the title and
// description, the changed files, and a contracts index of the symbols the
// pull request removes or changes with where they are still used. The
// bundle is the first part of every review call in a run, so providers
// cache it across file reviews.
type ContextSettings struct {
	// MaxIndexChars bounds the contracts index; 0 means
	// DefaultIndexChars.
	MaxIndexChars int `json:"max_index_chars,omitempty"`
	// MaxCallers bounds the uses listed per symbol; 0 means
	// DefaultMaxCallers.
	MaxCallers int `json:"max_callers,omitempty"`
	// WholePRChars bounds the diff one whole_pr call reads; 0 means
	// DefaultWholePRChars.
	WholePRChars int `json:"whole_pr_chars,omitempty"`
	// RepoTools lets whole_pr agents read files, search and list
	// directories of the checked-out repository, at most MaxToolSteps
	// model calls per review call (0 means DefaultToolSteps).
	RepoTools    bool `json:"repo_tools,omitempty"`
	MaxToolSteps int  `json:"max_tool_steps,omitempty"`
}

// Context defaults and limits.
const (
	DefaultIndexChars   = 10000
	DefaultMaxCallers   = 8
	DefaultWholePRChars = 200000
	DefaultToolSteps    = 12
	maxToolSteps        = 40
)

// IndexChars is the contracts index budget.
func (settings *ContextSettings) IndexChars() int {
	if settings == nil || settings.MaxIndexChars == 0 {
		return DefaultIndexChars
	}
	return settings.MaxIndexChars
}

// Callers is how many uses are listed per symbol.
func (settings *ContextSettings) Callers() int {
	if settings == nil || settings.MaxCallers == 0 {
		return DefaultMaxCallers
	}
	return settings.MaxCallers
}

// WholePRBudget is how much diff one whole_pr call reads.
func (settings *ContextSettings) WholePRBudget() int {
	if settings == nil || settings.WholePRChars == 0 {
		return DefaultWholePRChars
	}
	return settings.WholePRChars
}

// ToolSteps is how many model calls a whole_pr call with tools may make.
func (settings *ContextSettings) ToolSteps() int {
	if settings == nil || settings.MaxToolSteps == 0 {
		return DefaultToolSteps
	}
	return settings.MaxToolSteps
}

func (settings *ContextSettings) validate() error {
	if settings == nil {
		return nil
	}
	switch {
	case settings.MaxIndexChars != 0 && (settings.MaxIndexChars < 1000 || settings.MaxIndexChars > 60000):
		return errors.New("context.max_index_chars must be between 1000 and 60000")
	case settings.MaxCallers < 0 || settings.MaxCallers > 50:
		return errors.New("context.max_callers must be between 1 and 50")
	case settings.WholePRChars != 0 && (settings.WholePRChars < MinFileChars || settings.WholePRChars > 1000000):
		return fmt.Errorf("context.whole_pr_chars must be between %d and 1000000", MinFileChars)
	case settings.MaxToolSteps < 0 || settings.MaxToolSteps > maxToolSteps:
		return fmt.Errorf("context.max_tool_steps must be between 2 and %d", maxToolSteps)
	case settings.MaxToolSteps == 1:
		return errors.New("context.max_tool_steps must be at least 2 so the agent can answer after a tool call")
	case !settings.RepoTools && settings.MaxToolSteps != 0:
		return errors.New("context.max_tool_steps needs repo_tools")
	}
	return nil
}

// MaxRunCostLimit bounds the per-run cost cap.
const MaxRunCostLimit = 1000

func (policy ReviewPolicy) checkPolicy(kind string) error {
	if policy == (ReviewPolicy{}) {
		return nil
	}
	if kind != "pr_review" {
		return errors.New("only PR review workflows have cost caps, tiers, context or re-review settings")
	}
	if math.IsNaN(policy.MaxRunCostUSD) || policy.MaxRunCostUSD < 0 || policy.MaxRunCostUSD > MaxRunCostLimit {
		return fmt.Errorf("max_run_cost_usd must be between 0 and %d", MaxRunCostLimit)
	}
	return policy.Context.validate()
}
