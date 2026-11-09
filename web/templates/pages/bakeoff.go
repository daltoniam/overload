package pages

import (
	"fmt"
	"slices"
	"time"

	"github.com/daltoniam/overload/postgres"
)

// ModelSummary totals one model's results in a bake-off.
type ModelSummary struct {
	Model        string
	Reviews      int
	Failed       int
	Expected     int
	Matched      int
	Misplaced    int
	Extra        int
	InputTokens  int64
	OutputTokens int64
	Duration     time.Duration
}

// Found formats matched of expected.
func (summary ModelSummary) Found() string {
	return fmt.Sprintf("%d of %d", summary.Matched, summary.Expected)
}

// AverageTime is the mean time per review.
func (summary ModelSummary) AverageTime() string {
	if summary.Reviews-summary.Failed <= 0 {
		return "n/a"
	}
	return (summary.Duration / time.Duration(summary.Reviews-summary.Failed)).Round(time.Second).String()
}

// Summarize totals a bake-off's results per model, in the order the
// models were chosen.
func Summarize(bakeoff postgres.Bakeoff) []ModelSummary {
	var summaries []ModelSummary
	for _, model := range bakeoff.Models {
		summary := ModelSummary{Model: model.Name}
		for _, result := range bakeoff.Results {
			if result.Model != model.Name {
				continue
			}
			summary.Reviews++
			summary.Expected += result.Score.Expected
			if result.Error != "" {
				summary.Failed++
				continue
			}
			summary.Matched += result.Score.Matched
			summary.Misplaced += result.Score.Misplaced
			summary.Extra += result.Score.Unmatched
			summary.InputTokens += result.InputTokens
			summary.OutputTokens += result.OutputTokens
			summary.Duration += result.Duration
		}
		summaries = append(summaries, summary)
	}
	slices.SortStableFunc(summaries, func(a, b ModelSummary) int {
		return b.Matched*max(a.Expected, 1) - a.Matched*max(b.Expected, 1)
	})
	return summaries
}

func caseCount(count int) string {
	if count == 1 {
		return "1 case"
	}
	return fmt.Sprintf("%d cases", count)
}
