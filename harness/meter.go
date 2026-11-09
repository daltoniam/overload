package harness

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"charm.land/fantasy"
	"github.com/daltoniam/overload"
)

// errCostCap is returned for a model call the run's cost cap leaves no
// room for. The call is not made.
var errCostCap = errors.New("cost cap reached")

// runBudget tracks what one review spends across its concurrent model
// calls and enforces the workflow's per-run cap. Before a call starts it
// reserves an upper estimate of the call's cost; a call whose estimate does
// not fit under the cap with everything spent and reserved is refused.
// Calls already in flight finish, and their actual cost replaces the
// reservation.
type runBudget struct {
	limit float64 // 0 means no cap

	mu       sync.Mutex
	spent    overload.Spend
	usage    overload.Usage
	reserved float64
	refused  int
}

func newRunBudget(limit float64) *runBudget {
	return &runBudget{limit: limit}
}

func (budget *runBudget) totals() (overload.Usage, overload.Spend, int) {
	if budget == nil {
		return overload.Usage{}, overload.Spend{}, 0
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.usage, budget.spent, budget.refused
}

// reserve claims room for a call estimated to cost estimate dollars. A
// model without prices cannot be capped, so its calls always run.
func (budget *runBudget) reserve(estimate float64, priced bool) error {
	if budget == nil || budget.limit <= 0 || !priced {
		return nil
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.spent.USD+budget.reserved+estimate > budget.limit {
		budget.refused++
		return errCostCap
	}
	budget.reserved += estimate
	return nil
}

func (budget *runBudget) settle(estimate float64, priced bool, usage overload.Usage, spend overload.Spend) {
	if budget == nil {
		return
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.limit > 0 && priced {
		budget.reserved -= estimate
	}
	budget.usage = budget.usage.Add(usage)
	budget.spent = budget.spent.Add(spend)
}

// meter adds up the calls of one agent (or of the planner or verifier) and
// charges each to the run's budget. It is safe for concurrent use; a nil
// meter records nothing and enforces nothing.
type meter struct {
	run *runBudget

	mu    sync.Mutex
	usage overload.Usage
	spend overload.Spend
}

func newMeter(run *runBudget) *meter {
	return &meter{run: run}
}

func (m *meter) totals() (overload.Usage, overload.Spend) {
	if m == nil {
		return overload.Usage{}, overload.Spend{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage, m.spend
}

// usageOf converts a provider's usage to overload's, where input counts
// every prompt token. Fantasy reports cache reads and cache writes apart
// from uncached input.
func usageOf(usage fantasy.Usage) overload.Usage {
	return overload.Usage{Calls: 1, Input: usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens, CachedInput: usage.CacheReadTokens, CacheWrite: usage.CacheCreationTokens, Output: usage.OutputTokens}
}

// charsPerToken estimates prompt tokens from characters for reservations.
// Code and diffs run near 3.5 characters per token; 3 keeps the estimate
// above most real counts.
const charsPerToken = 3

// estimate is an upper estimate of one model call's cost: the whole
// prompt as uncached input and the full output budget.
func (plan reasoningPlan) estimate(call fantasy.AgentCall) (float64, bool) {
	chars := plan.systemChars + len(call.Prompt)
	for _, message := range call.Messages {
		for _, part := range message.Content {
			if text, ok := part.(fantasy.TextPart); ok {
				chars += len(text.Text)
			}
		}
	}
	return plan.estimateChars(chars)
}

func (plan reasoningPlan) estimateChars(chars int) (float64, bool) {
	return plan.pricing.Cost(overload.Usage{Input: int64(chars/charsPerToken + 1), Output: plan.maxTokens})
}

// messageChars measures a step's prompt, tool calls and results included,
// leaving out system messages, which the caller counts once. The JSON form
// is a little longer than the text, which keeps the estimate on the high
// side.
func messageChars(messages []fantasy.Message) int {
	messages = slices.DeleteFunc(slices.Clone(messages), func(message fantasy.Message) bool { return message.Role == fantasy.MessageRoleSystem })
	data, err := json.Marshal(messages)
	if err != nil {
		chars := 0
		for _, message := range messages {
			for _, part := range message.Content {
				if text, ok := part.(fantasy.TextPart); ok {
					chars += len(text.Text)
				}
			}
		}
		return chars
	}
	return len(data)
}

// call runs one model call through the budget: it reserves room, makes the
// call, and records what it used. A call that may take several steps
// (with tools) reserves and settles each step on its own, estimated from
// that step's prompt, so the context growing with earlier answers and
// tool results is accounted for before each step starts.
func (m *meter) call(ctx context.Context, plan reasoningPlan, call fantasy.AgentCall, generate func(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error)) (*fantasy.AgentResult, error) {
	if m == nil {
		return generate(ctx, call)
	}
	if plan.steps > 1 {
		return m.callSteps(ctx, plan, call, generate)
	}
	estimate, priced := plan.estimate(call)
	if err := m.run.reserve(estimate, priced); err != nil {
		return nil, err
	}
	response, err := generate(ctx, call)
	var usage overload.Usage
	var spend overload.Spend
	switch {
	case response != nil && len(response.Steps) > 1:
		// Each step is a model call, priced on its own: long-context
		// prices depend on one call's prompt.
		for _, step := range response.Steps {
			stepUsage := usageOf(step.Usage)
			usage = usage.Add(stepUsage)
			spend = spend.Charge(plan.pricing, stepUsage)
		}
	case response != nil:
		usage = usageOf(response.TotalUsage)
		spend = spend.Charge(plan.pricing, usage)
	default:
		usage.Calls = 1
		spend = spend.Charge(plan.pricing, usage)
	}
	m.record(usage, spend)
	m.run.settle(estimate, priced, usage, spend)
	return response, err
}

// callSteps is call for a generate that may take several model steps.
func (m *meter) callSteps(ctx context.Context, plan reasoningPlan, call fantasy.AgentCall, generate func(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error)) (*fantasy.AgentResult, error) {
	_, priced := plan.estimateChars(0)
	reserved, inFlight, settled := 0.0, false, 0
	settle := func(steps []fantasy.StepResult) {
		for _, step := range steps[min(settled, len(steps)):] {
			usage := usageOf(step.Usage)
			spend := overload.Spend{}.Charge(plan.pricing, usage)
			m.record(usage, spend)
			m.run.settle(reserved, priced, usage, spend)
			reserved, inFlight = 0, false
			settled++
		}
	}
	inner := call.PrepareStep
	call.PrepareStep = func(ctx context.Context, options fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		settle(options.Steps)
		var prepared fantasy.PrepareStepResult
		if inner != nil {
			var err error
			if ctx, prepared, err = inner(ctx, options); err != nil {
				return ctx, prepared, err
			}
		}
		messages := options.Messages
		if prepared.Messages != nil {
			messages = prepared.Messages
		}
		// The agent's system prompt, or the step's replacement for it.
		chars := messageChars(messages) + plan.systemChars
		if prepared.System != nil {
			chars += len(*prepared.System) - plan.systemChars
		}
		estimate, _ := plan.estimateChars(chars)
		if err := m.run.reserve(estimate, priced); err != nil {
			return ctx, prepared, err
		}
		reserved, inFlight = estimate, true
		return ctx, prepared, nil
	}
	response, err := generate(ctx, call)
	if response != nil {
		settle(response.Steps)
	}
	if inFlight {
		// The step failed without a result: count the call and free its
		// reservation.
		usage := overload.Usage{Calls: 1}
		spend := overload.Spend{}.Charge(plan.pricing, usage)
		m.record(usage, spend)
		m.run.settle(reserved, priced, usage, spend)
	}
	return response, err
}

func (m *meter) record(usage overload.Usage, spend overload.Spend) {
	m.mu.Lock()
	m.usage = m.usage.Add(usage)
	m.spend = m.spend.Add(spend)
	m.mu.Unlock()
}
