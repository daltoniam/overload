package harness

import (
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	openaisdk "github.com/charmbracelet/openai-go"
	"github.com/charmbracelet/openai-go/packages/param"
)

func hostedPrepareCall(model fantasy.LanguageModel, params *openaisdk.ChatCompletionNewParams, call fantasy.Call) ([]fantasy.CallWarning, error) {
	warnings, err := openaicompat.PrepareCallFunc(model, params, call)
	if err != nil {
		return warnings, err
	}
	if call.MaxOutputTokens != nil {
		params.MaxTokens = param.Opt[int64]{}
		params.MaxCompletionTokens = param.NewOpt(*call.MaxOutputTokens)
	}
	return warnings, nil
}
