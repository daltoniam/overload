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
	return nil
}
