package eval

import (
	"cmp"
	"encoding/csv"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
)

// Label marks a known bug in a past pull request: a finding counts as
// catching it when it is on Path within Window lines of Line and its text
// mentions one of Keywords (any finding at that place when there are no
// keywords).
type Label struct {
	PR       int      `json:"pr"`
	Path     string   `json:"path"`
	Line     int      `json:"line"`
	Window   int      `json:"window"`
	Keywords []string `json:"keywords,omitempty"`
}

// RunResult is one workflow's dry-run review of one pull request.
type RunResult struct {
	PR       int                `json:"pr"`
	Workflow string             `json:"workflow"`
	RunID    int64              `json:"run_id"`
	Status   string             `json:"status"`
	Error    string             `json:"error,omitempty"`
	Findings []overload.Finding `json:"findings"`
	Dropped  int                `json:"dropped,omitempty"`
	CostUSD  float64            `json:"cost_usd"`
	Unpriced int64              `json:"unpriced_calls,omitempty"`
	Seconds  float64            `json:"seconds"`
	Tier     string             `json:"tier,omitempty"`
	Degraded []string           `json:"degraded,omitempty"`
}

// LabelResult says which workflows caught a labeled bug.
type LabelResult struct {
	Label
	CaughtBy []string `json:"caught_by"`
}

// WorkflowSummary compares workflows: recall over the labeled bugs (a
// failed run catches nothing), findings per pull request as a proxy for
// noise, and cost and time per pull request.
type WorkflowSummary struct {
	Workflow      string  `json:"workflow"`
	Runs          int     `json:"runs"`
	Completed     int     `json:"completed"`
	Failed        int     `json:"failed"`
	Degraded      int     `json:"degraded"`
	Labels        int     `json:"labels"`
	Caught        int     `json:"caught"`
	Recall        float64 `json:"recall"`
	FindingsPerPR float64 `json:"findings_per_pr"`
	CostTotalUSD  float64 `json:"cost_total_usd"`
	CostP50USD    float64 `json:"cost_p50_usd"`
	CostP90USD    float64 `json:"cost_p90_usd"`
	SecondsP50    float64 `json:"seconds_p50"`
	SecondsP90    float64 `json:"seconds_p90"`
	UnpricedRuns  int     `json:"unpriced_runs,omitempty"`
}

// Report is the outcome of evaluating workflows over past pull requests.
type Report struct {
	Repository string            `json:"repository"`
	PRs        []int             `json:"prs"`
	Workflows  []string          `json:"workflows"`
	Summary    []WorkflowSummary `json:"summary"`
	Labels     []LabelResult     `json:"labels,omitempty"`
	Runs       []RunResult       `json:"runs"`
}

// Catches reports whether finding catches label.
func (label Label) Catches(finding overload.Finding) bool {
	if finding.Path != label.Path {
		return false
	}
	if distance := finding.Line - label.Line; distance > max(label.Window, 0) || -distance > max(label.Window, 0) {
		return false
	}
	if len(label.Keywords) == 0 {
		return true
	}
	text := strings.ToLower(strings.Join([]string{finding.Title, finding.Body, finding.Evidence, finding.Category}, "\n"))
	for _, keyword := range label.Keywords {
		if keyword = strings.ToLower(strings.TrimSpace(keyword)); keyword != "" && strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

// BuildReport summarizes runs per workflow in the given order and scores
// them against labels. Runs are listed by pull request, then workflow.
func BuildReport(repository string, prs []int, workflows []string, runs []RunResult, labels []Label) Report {
	report := Report{Repository: repository, PRs: slices.Clone(prs), Workflows: slices.Clone(workflows), Runs: slices.Clone(runs)}
	order := map[string]int{}
	for index, name := range workflows {
		order[name] = index
	}
	slices.SortStableFunc(report.Runs, func(a, b RunResult) int {
		if a.PR != b.PR {
			return cmp.Compare(a.PR, b.PR)
		}
		return cmp.Compare(order[a.Workflow], order[b.Workflow])
	})
	inBatch := map[int]bool{}
	for _, pr := range prs {
		inBatch[pr] = true
	}
	var scored []Label
	for _, label := range labels {
		if inBatch[label.PR] {
			scored = append(scored, label)
		}
	}
	for _, label := range scored {
		result := LabelResult{Label: label, CaughtBy: []string{}}
		for _, name := range workflows {
			for _, run := range report.Runs {
				if run.Workflow == name && run.PR == label.PR && run.Status == "completed" && slices.ContainsFunc(run.Findings, label.Catches) {
					result.CaughtBy = append(result.CaughtBy, name)
					break
				}
			}
		}
		report.Labels = append(report.Labels, result)
	}
	for _, name := range workflows {
		summary := WorkflowSummary{Workflow: name, Labels: len(scored)}
		var costs, seconds []float64
		findings := 0
		for _, run := range report.Runs {
			if run.Workflow != name {
				continue
			}
			summary.Runs++
			switch run.Status {
			case "completed":
				summary.Completed++
			case "failed":
				summary.Failed++
			}
			if len(run.Degraded) > 0 {
				summary.Degraded++
			}
			if run.Unpriced > 0 {
				summary.UnpricedRuns++
			}
			findings += len(run.Findings)
			summary.CostTotalUSD += run.CostUSD
			costs = append(costs, run.CostUSD)
			seconds = append(seconds, run.Seconds)
		}
		for _, label := range report.Labels {
			if slices.Contains(label.CaughtBy, name) {
				summary.Caught++
			}
		}
		if summary.Labels > 0 {
			summary.Recall = float64(summary.Caught) / float64(summary.Labels)
		}
		if summary.Runs > 0 {
			summary.FindingsPerPR = float64(findings) / float64(summary.Runs)
		}
		summary.CostP50USD, summary.CostP90USD = overload.Percentile(costs, 50), overload.Percentile(costs, 90)
		summary.SecondsP50, summary.SecondsP90 = overload.Percentile(seconds, 50), overload.Percentile(seconds, 90)
		report.Summary = append(report.Summary, summary)
	}
	return report
}

// WriteCSV writes one row per workflow summary.
func (report Report) WriteCSV(out io.Writer) error {
	writer := csv.NewWriter(out)
	rows := [][]string{{"workflow", "runs", "completed", "failed", "degraded", "labels", "caught", "recall", "findings_per_pr", "cost_total_usd", "cost_p50_usd", "cost_p90_usd", "seconds_p50", "seconds_p90", "unpriced_runs"}}
	number := func(value float64, decimals int) string { return strconv.FormatFloat(value, 'f', decimals, 64) }
	for _, summary := range report.Summary {
		rows = append(rows, []string{summary.Workflow, strconv.Itoa(summary.Runs), strconv.Itoa(summary.Completed), strconv.Itoa(summary.Failed), strconv.Itoa(summary.Degraded), strconv.Itoa(summary.Labels), strconv.Itoa(summary.Caught), number(summary.Recall, 3), number(summary.FindingsPerPR, 2), number(summary.CostTotalUSD, 4), number(summary.CostP50USD, 4), number(summary.CostP90USD, 4), number(summary.SecondsP50, 1), number(summary.SecondsP90, 1), strconv.Itoa(summary.UnpricedRuns)})
	}
	if err := writer.WriteAll(rows); err != nil {
		return fmt.Errorf("write CSV: %w", err)
	}
	return nil
}
