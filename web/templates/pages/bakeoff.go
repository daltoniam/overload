package pages

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/daltoniam/overload/postgres"
)

// ModelSummary totals one model's results in a bake-off. Failed reviews
// count only in Reviews and Failed.
type ModelSummary struct {
	Model            string
	Reviews          int
	Failed           int
	Expected         int
	Matched          int
	Misplaced        int
	PlacementChecked int
	Extra            int
	InputTokens      int64
	OutputTokens     int64
	Duration         time.Duration
}

func (summary ModelSummary) completed() int {
	return summary.Reviews - summary.Failed
}

// Found formats bugs matched of bugs expected in completed reviews.
func (summary ModelSummary) Found() string {
	if summary.completed() <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d of %d (%s)", summary.Matched, summary.Expected, reviewCount(summary.completed()))
}

func reviewCount(count int) string {
	if count == 1 {
		return "1 review"
	}
	return fmt.Sprintf("%d reviews", count)
}

// MisplacedCount is n/a when no completed review had a bug with expected
// words, the only kind a misplaced finding can be recognised by.
func (summary ModelSummary) MisplacedCount() string {
	if summary.completed() <= 0 || summary.PlacementChecked == 0 {
		return "n/a"
	}
	return strconv.Itoa(summary.Misplaced)
}

// ExtraCount formats the findings that matched no expected bug.
func (summary ModelSummary) ExtraCount() string {
	if summary.completed() <= 0 {
		return "n/a"
	}
	return strconv.Itoa(summary.Extra)
}

// AverageTime is the mean time per completed review.
func (summary ModelSummary) AverageTime() string {
	if summary.completed() <= 0 {
		return "n/a"
	}
	return (summary.Duration / time.Duration(summary.completed())).Round(time.Second).String()
}

// Summarize totals a bake-off's results per model, sorted by the share of
// expected bugs found, best first; equal shares go to the model with more
// completed reviews, then keep the order the models were chosen in.
func Summarize(bakeoff postgres.Bakeoff) []ModelSummary {
	var summaries []ModelSummary
	for _, model := range bakeoff.Models {
		summary := ModelSummary{Model: model.Name}
		for _, result := range bakeoff.Results {
			if result.Model != model.Name {
				continue
			}
			summary.Reviews++
			if result.Error != "" {
				summary.Failed++
				continue
			}
			summary.Expected += result.Score.Expected
			summary.Matched += result.Score.Matched
			summary.Misplaced += result.Score.Misplaced
			summary.PlacementChecked += result.Score.PlacementChecked
			summary.Extra += result.Score.Unmatched
			summary.InputTokens += result.InputTokens
			summary.OutputTokens += result.OutputTokens
			summary.Duration += result.Duration
		}
		summaries = append(summaries, summary)
	}
	slices.SortStableFunc(summaries, func(a, b ModelSummary) int {
		if share := b.Matched*max(a.Expected, 1) - a.Matched*max(b.Expected, 1); share != 0 {
			return share
		}
		return b.completed() - a.completed()
	})
	return summaries
}

// misplacedResult is n/a for a case without expected words.
func misplacedResult(result postgres.BakeoffResult) string {
	if result.Score.PlacementChecked == 0 {
		return "n/a"
	}
	return strconv.Itoa(result.Score.Misplaced)
}

func caseCount(count int) string {
	if count == 1 {
		return "1 case"
	}
	return fmt.Sprintf("%d cases", count)
}
