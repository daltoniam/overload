package overload

import (
	"errors"
	"fmt"
	"slices"
)

// Risk tiers of a pull request, from its size and the paths it touches.
// Sub-agents can be limited to tiers, so a one-line docs change does not
// get the whole specialist cohort.
const (
	RiskTrivial = "trivial"
	RiskLite    = "lite"
	RiskFull    = "full"
)

// Lines changed (added plus removed, skip paths aside) that move a pull
// request from trivial to lite and from lite to full.
const (
	TrivialMaxLines = 20
	LiteMaxLines    = 400
)

// MaxReviewTokensLimit bounds a workflow's token budget per review.
const MaxReviewTokensLimit = 100_000_000

var riskTiers = []string{RiskTrivial, RiskLite, RiskFull}

// AssessRisk is the tier of a pull request that changes lines lines and
// touches a sensitive path or not. Sensitive paths are always full.
func AssessRisk(lines int, sensitive bool) string {
	switch {
	case sensitive || lines > LiteMaxLines:
		return RiskFull
	case lines > TrivialMaxLines:
		return RiskLite
	default:
		return RiskTrivial
	}
}

func validateTiers(tiers []string) error {
	for _, tier := range tiers {
		if !slices.Contains(riskTiers, tier) {
			return fmt.Errorf("tier %q must be trivial, lite or full", tier)
		}
	}
	return nil
}

func validateCohort(kind string, sensitive []string, budget int) error {
	if len(sensitive) == 0 && budget == 0 {
		return nil
	}
	if kind != "pr_review" {
		return errors.New("only PR review workflows have sensitive paths or a token budget")
	}
	if budget < 0 || budget > MaxReviewTokensLimit {
		return fmt.Errorf("max_review_tokens must be between 0 and %d", MaxReviewTokensLimit)
	}
	if err := validateGlobs(sensitive); err != nil {
		return fmt.Errorf("sensitive paths: %w", err)
	}
	return nil
}
