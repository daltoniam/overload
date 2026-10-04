package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/review"
)

func reviewConcurrency(configured, files int) int {
	workers := max(configured, 1)
	return min(workers, max(files, 1))
}

// forEachFile calls review for every index with at most concurrency calls in
// flight, starting files in order. The first failure cancels the remaining
// work; its error is returned in preference to cancellations it caused.
func forEachFile(ctx context.Context, files, concurrency int, review func(context.Context, int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	next := make(chan int)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		firstErr = preferError(firstErr, err)
		cancel()
	}
	for range reviewConcurrency(concurrency, files) {
		wg.Go(func() {
			for index := range next {
				if err := review(ctx, index); err != nil {
					fail(err)
				}
			}
		})
	}
feed:
	for index := range files {
		select {
		case next <- index:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// preferError keeps the first real failure over cancellations it caused.
func preferError(current, next error) error {
	if current == nil || (errors.Is(current, context.Canceled) && !errors.Is(next, context.Canceled)) {
		return next
	}
	return current
}

type reviewTask struct {
	agent, file int
}

// connectionGroup is the work sent to one model server. Agents that share a
// server share its parallel limit, so several agents on one local model
// queue behind each other while agents on other servers run alongside.
type connectionGroup struct {
	limit int
	tasks []reviewTask
}

func connectionKey(model overload.ModelProfile) string {
	return model.BaseURL + "\x00" + model.Model
}

// runGroups runs every group at the same time, each with at most its limit
// of tasks in flight, starting tasks in order. The first failure cancels all
// remaining work.
func runGroups(ctx context.Context, groups []connectionGroup, run func(context.Context, reviewTask) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, group := range groups {
		wg.Go(func() {
			err := forEachFile(ctx, len(group.tasks), group.limit, func(ctx context.Context, index int) error {
				return run(ctx, group.tasks[index])
			})
			if err != nil {
				mu.Lock()
				firstErr = preferError(firstErr, err)
				mu.Unlock()
				cancel()
			}
		})
	}
	wg.Wait()
	return firstErr
}

type fileOutcome struct {
	findings     []overload.Finding
	summary      string
	raw          int
	validated    int
	inputTokens  int64
	cachedTokens int64
	outputTokens int64
	fallback     bool
}

// addUsage counts cached prompt tokens as input too: providers report them
// separately, and leaving them out undercounts the prompt actually sent.
func (outcome *fileOutcome) addUsage(usage fantasy.Usage) {
	outcome.inputTokens += usage.InputTokens + usage.CacheReadTokens
	outcome.cachedTokens += usage.CacheReadTokens
	outcome.outputTokens += usage.OutputTokens
}

// reviewFile reviews one changed file, repairing malformed JSON once, and
// returns the validated findings that belong to that file.
func reviewFile(ctx context.Context, agent fantasy.Agent, plan reasoningPlan, bundle, path, diff string) (fileOutcome, error) {
	var outcome fileOutcome
	response, fallback, err := generateReview(ctx, agent, plan, fantasy.AgentCall{Prompt: bundle})
	if err != nil {
		return outcome, fmt.Errorf("review incomplete at %s: %w", path, err)
	}
	outcome.fallback = fallback
	outcome.addUsage(response.TotalUsage)
	text := strings.TrimSpace(response.Response.Content.Text())
	var fileResult overload.ReviewResult
	if err := json.Unmarshal([]byte(cleanModelJSON(text)), &fileResult); err != nil {
		if len(text) > 12000 {
			text = text[:12000]
		}
		response, err = agent.Generate(ctx, fantasy.AgentCall{Prompt: repairPrompt + text})
		if err != nil {
			return outcome, fmt.Errorf("review incomplete repairing %s: %w", path, err)
		}
		outcome.addUsage(response.TotalUsage)
		if err := json.Unmarshal([]byte(cleanModelJSON(response.Response.Content.Text())), &fileResult); err != nil {
			return outcome, fmt.Errorf("review incomplete at %s: model_output_invalid: %w", path, err)
		}
	}
	validated, err := review.Validate(fileResult.Findings, diff, 10)
	if err != nil {
		return outcome, err
	}
	outcome.raw = len(fileResult.Findings)
	outcome.validated = len(validated)
	outcome.summary = fileResult.Summary
	for _, finding := range validated {
		if finding.Path == path {
			outcome.findings = append(outcome.findings, finding)
		}
	}
	return outcome, nil
}

// merge appends one agent's per-file outcomes to result in file order, so
// output does not depend on which file finished first. Duplicates across
// agents are collapsed afterwards by review.Validate, which keeps every
// agent that reported a finding.
func merge(result *overload.ReviewResult, summaries *[]string, outcomes []fileOutcome) {
	for _, outcome := range outcomes {
		result.Findings = append(result.Findings, outcome.findings...)
		if len(outcome.findings) > 0 && outcome.summary != "" {
			*summaries = append(*summaries, outcome.summary)
		}
		result.Metrics["raw_candidates"] = metricCount(result.Metrics, "raw_candidates") + outcome.raw
		result.Metrics["validated_candidates"] = metricCount(result.Metrics, "validated_candidates") + outcome.validated
		result.Metrics["input_tokens"] = metricTokens(result.Metrics, "input_tokens") + outcome.inputTokens
		result.Metrics["output_tokens"] = metricTokens(result.Metrics, "output_tokens") + outcome.outputTokens
		result.Metrics["cached_input_tokens"] = metricTokens(result.Metrics, "cached_input_tokens") + outcome.cachedTokens
		if outcome.fallback {
			result.Metrics["effort_fallbacks"] = metricCount(result.Metrics, "effort_fallbacks") + 1
		}
	}
}
