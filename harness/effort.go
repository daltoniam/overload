package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/daltoniam/overload"
)

const (
	localReviewEffort     = "xhigh"
	localFallbackEffort   = "medium"
	localMaxOutputTokens  = 49152
	hostedMaxOutputTokens = 16384
)

// Model call limits. A local model at about 10 tokens/s needs over an hour
// to use its whole output budget, so its limit is generous; it exists so a
// hung server fails one call (a sub-agent then ends the review degraded)
// instead of holding the review until the job's own timeout.
var (
	localCallTimeout  = 90 * time.Minute
	hostedCallTimeout = 15 * time.Minute
)

// reasoningPlan is how review requests to one model control thinking.
type reasoningPlan struct {
	provider  string
	api       string
	param     string
	effort    string
	maxTokens int64
	timeout   time.Duration
	// pricing prices the model's calls; system is the agent's system
	// prompt (its length feeds cost estimates). steps is how many model
	// calls one generate may make (more than 1 with tools).
	pricing     *overload.Pricing
	systemChars int
	system      string
	steps       int
}

// withSteps returns the plan for calls that may take steps model calls.
func (plan reasoningPlan) withSteps(steps int) reasoningPlan {
	plan.steps = steps
	return plan
}

func planFor(model overload.ModelProfile) reasoningPlan {
	hosted := model.ConnectionKind == "hosted"
	plan := reasoningPlan{provider: model.Provider, api: model.API, param: model.ReasoningParam, effort: model.ReasoningEffort, timeout: localCallTimeout, pricing: model.Pricing}
	if hosted {
		plan.timeout = hostedCallTimeout
	}
	if plan.param == overload.ReasoningAuto && !hosted {
		plan.param, plan.effort = overload.ReasoningChatTemplate, localReviewEffort
	}
	switch {
	case model.MaxOutputTokens > 0:
		plan.maxTokens = int64(model.MaxOutputTokens)
	case hosted:
		plan.maxTokens = hostedMaxOutputTokens
	default:
		plan.maxTokens = localMaxOutputTokens
	}
	return plan
}

func (plan reasoningPlan) label() string {
	if plan.param == overload.ReasoningAuto || plan.param == overload.ReasoningNone {
		return "none"
	}
	return plan.param + ":" + plan.effort
}

func (plan reasoningPlan) providerOptions(effort string) fantasy.ProviderOptions {
	if plan.provider == overload.ProviderAnthropic {
		// Adaptive thinking with output_config.effort; these models reject
		// a manual budget_tokens.
		if plan.param != overload.ReasoningEffortField {
			return nil
		}
		level := anthropic.Effort(effort)
		return anthropic.NewProviderOptions(&anthropic.ProviderOptions{Effort: &level})
	}
	switch plan.param {
	case overload.ReasoningChatTemplate:
		return openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{
			ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": effort}},
		})
	case overload.ReasoningEffortField:
		level := openai.ReasoningEffort(effort)
		if plan.api == overload.APIResponses {
			return nil
		}
		return openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{ReasoningEffort: &level})
	default:
		return nil
	}
}

func reviewAgentOptions(plan reasoningPlan, systemPrompt string) []fantasy.AgentOption {
	options := []fantasy.AgentOption{fantasy.WithSystemPrompt(systemPrompt), fantasy.WithMaxOutputTokens(plan.maxTokens)}
	if provider := plan.providerOptions(plan.effort); provider != nil {
		options = append(options, fantasy.WithProviderOptions(provider))
	}
	return options
}

func (plan reasoningPlan) canFallBack() bool {
	if plan.param != overload.ReasoningChatTemplate && plan.param != overload.ReasoningEffortField {
		return false
	}
	switch plan.effort {
	case "none", "minimal", "low", localFallbackEffort:
		return false
	}
	return true
}

// generateReview runs one review call. A model that spends its whole output
// budget thinking returns no answer; that call is retried once at medium
// effort, reported through the second return value. Both calls are charged
// to meter.
func generateReview(ctx context.Context, agent fantasy.Agent, plan reasoningPlan, call fantasy.AgentCall, meter *meter) (*fantasy.AgentResult, bool, error) {
	response, err := plan.generate(ctx, agent, call, meter)
	if err != nil || !plan.canFallBack() || response.Response.FinishReason != fantasy.FinishReasonLength {
		return response, false, err
	}
	retry := call
	retry.ProviderOptions = plan.providerOptions(localFallbackEffort)
	response, err = plan.generate(ctx, agent, retry, meter)
	return response, true, err
}

// errCallTimeout marks a model call that ran past its limit.
var errCallTimeout = errors.New("model call timed out")

// generate makes one model call within the plan's time limit, charged to
// meter (nil records nothing).
func (plan reasoningPlan) generate(ctx context.Context, agent fantasy.Agent, call fantasy.AgentCall, meter *meter) (*fantasy.AgentResult, error) {
	return meter.call(ctx, plan, call, func(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
		if plan.timeout <= 0 {
			return agent.Generate(ctx, call)
		}
		callCtx, cancel := context.WithTimeout(ctx, plan.timeout)
		defer cancel()
		response, err := agent.Generate(callCtx, call)
		if err != nil && ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return response, fmt.Errorf("%w after %s", errCallTimeout, plan.timeout)
		}
		return response, err
	})
}
