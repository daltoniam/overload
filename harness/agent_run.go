package harness

import (
	"context"

	"charm.land/fantasy"
	"github.com/daltoniam/overload"
)

// finalStepNotice replaces tools on an agent's last allowed step, so a run
// that reaches its step limit still ends with a report.
const finalStepNotice = "\n\nYou have reached this run's step limit. Tools are no longer available. Write your final report now: what you did, what changed, and what is left."

// AgentRun is the outcome of one tool-using agent.
type AgentRun struct {
	Text         string
	Steps        int
	ToolCalls    int
	InputTokens  int64
	OutputTokens int64
	// Spend is the priced cost of the run's model calls, one per step.
	Spend overload.Spend
	// HitStepLimit means the agent was still working when it ran out of
	// steps and was asked to report.
	HitStepLimit bool
}

// RunAgent lets a model call tools until it answers or uses maxSteps model
// calls. The caller's context bounds the whole run.
func RunAgent(ctx context.Context, model overload.ModelProfile, systemPrompt, prompt string, toolset *Toolset, maxSteps int) (AgentRun, error) {
	var options []fantasy.AgentOption
	if toolset != nil {
		options = append(options, fantasy.WithTools(toolset.Tools()...))
	}
	agent, plan, err := newAgent(ctx, model, systemPrompt, options...)
	if err != nil {
		return AgentRun{}, err
	}
	final := systemPrompt + finalStepNotice
	hitLimit := false
	call := fantasy.AgentCall{
		Prompt:   prompt,
		StopWhen: []fantasy.StopCondition{fantasy.StepCountIs(maxSteps)},
		PrepareStep: func(ctx context.Context, step fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
			if toolset == nil || step.StepNumber < maxSteps-1 {
				return ctx, fantasy.PrepareStepResult{}, nil
			}
			hitLimit = true
			return ctx, fantasy.PrepareStepResult{DisableAllTools: true, System: &final}, nil
		},
	}
	result, err := agent.Generate(ctx, call)
	if (result == nil || len(result.Steps) == 0) && plan.retriesWithThinkingBudget(err) {
		call.ProviderOptions = plan.providerOptions(plan.effort)
		result, err = agent.Generate(ctx, call)
	}
	run := AgentRun{HitStepLimit: hitLimit}
	if toolset != nil {
		run.ToolCalls = toolset.Calls()
	}
	if result != nil {
		run.Steps = len(result.Steps)
		run.Text = stripThinking(result.Response.Content.Text())
		run.InputTokens, run.OutputTokens = result.TotalUsage.InputTokens+result.TotalUsage.CacheReadTokens+result.TotalUsage.CacheCreationTokens, result.TotalUsage.OutputTokens
		for _, step := range result.Steps {
			run.Spend = run.Spend.Charge(model.Pricing, usageOf(step.Usage))
		}
	}
	return run, err
}
