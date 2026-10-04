package harness

import (
	"context"

	"charm.land/fantasy"
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

// reasoningPlan is how review requests to one model control thinking.
type reasoningPlan struct {
	param     string
	effort    string
	maxTokens int64
}

func planFor(model overload.ModelProfile) reasoningPlan {
	hosted := model.ConnectionKind == "hosted"
	plan := reasoningPlan{param: model.ReasoningParam, effort: model.ReasoningEffort}
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
	switch plan.param {
	case overload.ReasoningChatTemplate:
		return openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{
			ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": effort}},
		})
	case overload.ReasoningEffortField:
		level := openai.ReasoningEffort(effort)
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
// effort, reported through the second return value.
func generateReview(ctx context.Context, agent fantasy.Agent, plan reasoningPlan, call fantasy.AgentCall) (*fantasy.AgentResult, bool, error) {
	response, err := agent.Generate(ctx, call)
	if err != nil || !plan.canFallBack() || response.Response.FinishReason != fantasy.FinishReasonLength {
		return response, false, err
	}
	retry := call
	retry.ProviderOptions = plan.providerOptions(localFallbackEffort)
	response, err = agent.Generate(ctx, retry)
	return response, true, err
}
