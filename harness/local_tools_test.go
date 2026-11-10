package harness

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

type echoTool struct{ provider fantasy.ProviderOptions }

func (tool *echoTool) Info() fantasy.ToolInfo                          { return fantasy.ToolInfo{Name: "echo"} }
func (tool *echoTool) ProviderOptions() fantasy.ProviderOptions        { return tool.provider }
func (tool *echoTool) SetProviderOptions(opts fantasy.ProviderOptions) { tool.provider = opts }
func (tool *echoTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse(call.Input), nil
}

func TestLocalToolsetCapsCalls(t *testing.T) {
	set := NewLocalToolset([]fantasy.AgentTool{&echoTool{}}, 2)
	tool := set.Tools()[0]
	for call := 1; call <= 3; call++ {
		response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if limited := response.IsError; limited != (call == 3) {
			t.Fatalf("call %d: %+v", call, response)
		}
	}
	if set.Calls() != 3 || tool.Info().Name != "echo" {
		t.Fatalf("calls %d name %s", set.Calls(), tool.Info().Name)
	}
}
