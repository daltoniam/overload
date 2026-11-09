package harness

import (
	"context"
	"reflect"

	"charm.land/fantasy"
	"github.com/daltoniam/overload"
)

// run makes one agent call. Anthropic calls stream: the Anthropic SDK
// refuses non-streaming requests whose output limit could take more than
// ten minutes (about 21,000 tokens, or 8,192 for some models) and cuts every
// non-streaming request off at ten minutes, which thinking models reach on
// large files.
func run(ctx context.Context, agent fantasy.Agent, provider string, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	if provider != overload.ProviderAnthropic {
		return agent.Generate(ctx, call)
	}
	return agent.Stream(ctx, streamCall(call))
}

// streamCall copies every field of an agent call into a streaming call. The
// two types share their fields; TestStreamCallCopiesEveryField fails if
// Fantasy adds one to only the first.
func streamCall(call fantasy.AgentCall) fantasy.AgentStreamCall {
	var stream fantasy.AgentStreamCall
	from, to := reflect.ValueOf(call), reflect.ValueOf(&stream).Elem()
	for index := range from.NumField() {
		if field := to.FieldByName(from.Type().Field(index).Name); field.IsValid() && field.Type() == from.Field(index).Type() {
			field.Set(from.Field(index))
		}
	}
	return stream
}
