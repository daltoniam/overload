package harness

import (
	"context"
	"net/http"
	"os"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/openai-go/option"
	"github.com/daltoniam/overload"
)

// modelHTTPClient has no response-header timeout. Non-streaming calls send
// no headers until the whole answer, thinking included, is generated, which
// can take longer than the OpenAI SDK's 10-minute default for local models
// and for hosted reasoning models on large files. Each call is still bounded
// by its plan's timeout (local 90 minutes, hosted 15).
var modelHTTPClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 0
	return &http.Client{Transport: transport}
}()

func newProvider(model overload.ModelProfile) (fantasy.Provider, error) {
	if model.API == overload.APIResponses {
		options := []openai.Option{openai.WithBaseURL(model.BaseURL), openai.WithAPIKey(os.Getenv(model.APIKeyEnv)), openai.WithHeaders(model.Headers), openai.WithUseResponsesAPI(), openai.WithResponsesAPIFunc(func(string) bool { return true })}
		options = append(options, openai.WithHTTPClient(modelHTTPClient))
		// Fantasy only sends reasoning settings for model names it knows, so
		// set the field on every request instead.
		if model.ReasoningParam == overload.ReasoningEffortField {
			options = append(options, openai.WithSDKOptions(option.WithJSONSet("reasoning.effort", model.ReasoningEffort)))
		}
		return openai.New(options...)
	}
	options := []openaicompat.Option{openaicompat.WithBaseURL(model.BaseURL), openaicompat.WithAPIKey(os.Getenv(model.APIKeyEnv)), openaicompat.WithHeaders(model.Headers), openaicompat.WithHTTPClient(modelHTTPClient)}
	if model.ConnectionKind == "hosted" {
		options = append(options, openaicompat.WithLanguageModelOptions(openai.WithLanguageModelPrepareCallFunc(hostedPrepareCall)))
	}
	return openaicompat.New(options...)
}

// newAgent builds a model client for one agent, applying its headers,
// connection kind, reasoning style and output limit.
func newAgent(ctx context.Context, model overload.ModelProfile, systemPrompt string, extra ...fantasy.AgentOption) (fantasy.Agent, reasoningPlan, error) {
	if err := validateModelHeaders(model.Headers); err != nil {
		return nil, reasoningPlan{}, err
	}
	provider, err := newProvider(model)
	if err != nil {
		return nil, reasoningPlan{}, err
	}
	languageModel, err := provider.LanguageModel(ctx, model.Model)
	if err != nil {
		return nil, reasoningPlan{}, err
	}
	plan := planFor(model)
	return fantasy.NewAgent(languageModel, append(reviewAgentOptions(plan, systemPrompt), extra...)...), plan, nil
}

// Complete sends one prompt to a model with the same connection, reasoning
// and output settings reviews use, and returns the reply text.
func Complete(ctx context.Context, model overload.ModelProfile, systemPrompt, prompt string) (string, error) {
	agent, plan, err := newAgent(ctx, model, systemPrompt)
	if err != nil {
		return "", err
	}
	response, _, err := generateReview(ctx, agent, plan, fantasy.AgentCall{Prompt: prompt})
	if err != nil {
		return "", err
	}
	return response.Response.Content.Text(), nil
}
