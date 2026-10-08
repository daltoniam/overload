package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/daltoniam/overload"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpFixture is a Switchboard-like MCP server with search and execute
// tools that records the headers and arguments it receives.
type mcpFixture struct {
	server *httptest.Server
	mu     sync.Mutex
	auth   []string
	sess   []string
	calls  []string
}

func newMCPFixture(t *testing.T, executeOutput string) *mcpFixture {
	t.Helper()
	fixture := &mcpFixture{}
	server := mcp.NewServer(&mcp.Implementation{Name: "switchboard-test", Version: "1"}, nil)
	type searchInput struct {
		Query string `json:"query" jsonschema:"keywords"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "search", Description: "Search available tools."}, func(ctx context.Context, request *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, any, error) {
		fixture.mu.Lock()
		fixture.calls = append(fixture.calls, "search:"+input.Query)
		fixture.mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "github_list_issues"}}}, nil, nil
	})
	type executeInput struct {
		ToolName string `json:"tool_name" jsonschema:"tool to run"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "execute", Description: "Execute a tool."}, func(ctx context.Context, request *mcp.CallToolRequest, input executeInput) (*mcp.CallToolResult, any, error) {
		fixture.mu.Lock()
		fixture.calls = append(fixture.calls, "execute:"+input.ToolName)
		fixture.mu.Unlock()
		if input.ToolName == "fail" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "unknown tool"}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: executeOutput}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "history", Description: "Recent calls; no arguments."}, func(ctx context.Context, request *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "[]"}}}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.auth = append(fixture.auth, r.Header.Get("Authorization"))
		fixture.sess = append(fixture.sess, r.Header.Get(SwitchboardSessionHeader))
		fixture.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sb_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *mcpFixture) toolServer() overload.ToolServer {
	return overload.ToolServer{Name: "sb", URL: fixture.server.URL, TokenEnv: "OVERLOAD_TOOL_TEST", Enabled: true}
}

// scriptedModel answers OpenAI chat requests from a list of replies and
// records each request's tool names and messages.
type scriptedModel struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []map[string]any
}

func newScriptedModel(t *testing.T, replies func(index int, request map[string]any) string) *scriptedModel {
	t.Helper()
	model := &scriptedModel{}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Errorf("model request: %v", err)
		}
		model.mu.Lock()
		index := len(model.requests)
		model.requests = append(model.requests, request)
		model.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, replies(index, request))
	}))
	t.Cleanup(model.server.Close)
	return model
}

func (model *scriptedModel) profile() overload.ModelProfile {
	return overload.ModelProfile{Name: "test", Provider: "openaicompat", ConnectionKind: "hosted", BaseURL: model.server.URL, Model: "test", APIKeyEnv: "OVERLOAD_MODEL_TEST"}
}

func toolCallReply(id, name, arguments string) string {
	encoded, _ := json.Marshal(arguments)
	return fmt.Sprintf(`{"id":"r","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, id, name, encoded)
}

func textReply(text string) string {
	encoded, _ := json.Marshal(text)
	return fmt.Sprintf(`{"id":"r","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%s}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, encoded)
}

func lastToolResult(request map[string]any) string {
	messages, _ := request["messages"].([]any)
	for index := len(messages) - 1; index >= 0; index-- {
		message, _ := messages[index].(map[string]any)
		if message["role"] == "tool" {
			content, _ := message["content"].(string)
			return content
		}
	}
	return ""
}

func TestRunAgentCallsMCPTools(t *testing.T) {
	t.Setenv("OVERLOAD_TOOL_TEST", "sb_test")
	t.Setenv("OVERLOAD_MODEL_TEST", "model-key")
	fixture := newMCPFixture(t, "issue #240 is open")
	model := newScriptedModel(t, func(index int, request map[string]any) string {
		switch index {
		case 0:
			return toolCallReply("call_1", "sb_search", `{"query":"github issues"}`)
		case 1:
			return toolCallReply("call_2", "sb_execute", `{"tool_name":"github_get_issue"}`)
		default:
			return textReply("Done: " + lastToolResult(request))
		}
	})
	var events []ToolEvent
	ctx := context.Background()
	toolset, err := ConnectTools(ctx, []overload.ToolServer{fixture.toolServer()}, "overload-run-7-research", 20, func(event ToolEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = toolset.Close() }()
	result, err := RunAgent(ctx, model.profile(), "Research.", "Scheduled input:\n{}", toolset, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Done: issue #240 is open" || result.Steps != 3 || result.ToolCalls != 2 || result.HitStepLimit {
		t.Fatalf("result = %+v", result)
	}
	if strings.Join(fixture.calls, ",") != "search:github issues,execute:github_get_issue" {
		t.Fatalf("calls = %v", fixture.calls)
	}
	if len(events) != 2 || events[1].Tool != "execute" || events[1].Output != "issue #240 is open" || events[1].IsError {
		t.Fatalf("events = %+v", events)
	}
	for index, auth := range fixture.auth {
		if auth != "Bearer sb_test" || fixture.sess[index] != "overload-run-7-research" {
			t.Fatalf("request %d auth=%q session=%q", index, auth, fixture.sess[index])
		}
	}
	tools, _ := model.requests[0]["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("model saw %d tools", len(tools))
	}
	for _, tool := range tools {
		parameters := tool.(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
		if _, isList := parameters["required"].([]any); !isList {
			t.Fatalf("tool schema %v: required must be a list, which OpenAI insists on", parameters)
		}
	}
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "sb_execute" && function["name"] != "sb_search" {
		t.Fatalf("tool names: %v", tools)
	}
}

func TestRunAgentStepLimitForcesReport(t *testing.T) {
	t.Setenv("OVERLOAD_TOOL_TEST", "sb_test")
	t.Setenv("OVERLOAD_MODEL_TEST", "model-key")
	fixture := newMCPFixture(t, "more")
	model := newScriptedModel(t, func(index int, request map[string]any) string {
		if _, hasTools := request["tools"]; !hasTools {
			return textReply("Report: ran out of steps")
		}
		return toolCallReply(fmt.Sprintf("call_%d", index), "sb_search", `{"query":"again"}`)
	})
	toolset, err := ConnectTools(context.Background(), []overload.ToolServer{fixture.toolServer()}, "", 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = toolset.Close() }()
	result, err := RunAgent(context.Background(), model.profile(), "Loop.", "go", toolset, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Steps != 3 || result.ToolCalls != 2 || !result.HitStepLimit || result.Text != "Report: ran out of steps" {
		t.Fatalf("result = %+v", result)
	}
	last := model.requests[len(model.requests)-1]
	if !strings.Contains(fmt.Sprint(last["messages"]), "step limit") {
		t.Fatal("final step did not ask for a report")
	}
}

func TestToolErrorsAndLimitsReachTheModel(t *testing.T) {
	t.Setenv("OVERLOAD_TOOL_TEST", "sb_test")
	fixture := newMCPFixture(t, strings.Repeat("x", maxToolOutputBytes+100))
	toolset, err := ConnectTools(context.Background(), []overload.ToolServer{fixture.toolServer()}, "", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = toolset.Close() }()
	execute := map[string]*mcpTool{}
	for _, tool := range toolset.Tools() {
		execute[tool.Info().Name] = tool.(*mcpTool)
	}
	cases := []struct {
		name, input string
		isError     bool
		contains    string
	}{
		{"remote error", `{"tool_name":"fail"}`, true, "unknown tool"},
		{"bad json", `not json`, true, "JSON object"},
		{"truncated", `{"tool_name":"big"}`, false, "output truncated"},
		{"call limit", `{"tool_name":"big"}`, true, "limit reached"},
	}
	for _, tc := range cases {
		response, err := execute["sb_execute"].Run(context.Background(), toolCall(tc.input))
		if err != nil || response.IsError != tc.isError || !strings.Contains(response.Content, tc.contains) {
			t.Errorf("%s: error=%v isError=%v content=%.120q", tc.name, err, response.IsError, response.Content)
		}
	}
}

func TestConnectToolsNeedsToken(t *testing.T) {
	fixture := newMCPFixture(t, "")
	t.Setenv("OVERLOAD_TOOL_TEST", "")
	if _, err := ConnectTools(context.Background(), []overload.ToolServer{fixture.toolServer()}, "", 5, nil); err == nil || !strings.Contains(err.Error(), "OVERLOAD_TOOL_TEST is not set") {
		t.Fatalf("missing token: %v", err)
	}
	t.Setenv("OVERLOAD_TOOL_TEST", "wrong")
	if _, err := ConnectTools(context.Background(), []overload.ToolServer{fixture.toolServer()}, "", 5, nil); err == nil {
		t.Fatal("wrong token accepted")
	}
	t.Setenv("OVERLOAD_TOOL_TEST", "sb_test")
	names, err := ListTools(context.Background(), fixture.toolServer())
	slices.Sort(names)
	if err != nil || strings.Join(names, ",") != "execute,history,search" {
		t.Fatalf("list tools: %v %v", names, err)
	}
}

func TestModelToolName(t *testing.T) {
	cases := map[[2]string]string{
		{"switchboard", "search"}:      "switchboard_search",
		{"sb", "github.list issues"}:   "sb_github_list_issues",
		{"s", strings.Repeat("a", 80)}: "s_" + strings.Repeat("a", 62),
		{"hosted-sb", "execute"}:       "hosted-sb_execute",
	}
	for in, want := range cases {
		if got := modelToolName(in[0], in[1]); got != want {
			t.Errorf("modelToolName(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func toolCall(input string) fantasy.ToolCall {
	return fantasy.ToolCall{ID: "call", Name: "sb_execute", Input: input}
}

func TestRunAgentResponsesAPIWithToolsAndReasoning(t *testing.T) {
	t.Setenv("OVERLOAD_TOOL_TEST", "sb_test")
	t.Setenv("OVERLOAD_MODEL_TEST", "model-key")
	fixture := newMCPFixture(t, "3 open issues")
	var paths []string
	model := newScriptedModel(t, func(index int, request map[string]any) string {
		if index == 0 {
			return `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-6.1-sol","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"sb_execute","arguments":"{\"tool_name\":\"github_list_issues\"}","status":"completed"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
		}
		return `{"id":"resp_2","object":"response","created_at":1,"status":"completed","model":"gpt-6.1-sol","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Found 3 open issues.","annotations":[]}]}],"usage":{"input_tokens":20,"output_tokens":6,"total_tokens":26}}`
	})
	model.server.Config.Handler = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	}(model.server.Config.Handler)
	profile := model.profile()
	profile.Model, profile.API, profile.ReasoningParam, profile.ReasoningEffort = "gpt-6.1-sol", overload.APIResponses, overload.ReasoningEffortField, "high"
	toolset, err := ConnectTools(context.Background(), []overload.ToolServer{fixture.toolServer()}, "", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = toolset.Close() }()
	result, err := RunAgent(context.Background(), profile, "Research.", "go", toolset, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Found 3 open issues." || result.ToolCalls != 1 || result.Steps != 2 {
		t.Fatalf("result = %+v", result)
	}
	if len(paths) != 2 || paths[0] != "/responses" {
		t.Fatalf("paths = %v, want the Responses API", paths)
	}
	first := model.requests[0]
	reasoning, _ := first["reasoning"].(map[string]any)
	tools, _ := first["tools"].([]any)
	if reasoning["effort"] != "high" || len(tools) != 3 {
		t.Fatalf("first request reasoning=%v tools=%d", first["reasoning"], len(tools))
	}
	if !strings.Contains(fmt.Sprint(model.requests[1]["input"]), "3 open issues") {
		t.Fatal("tool result was not sent back to the model")
	}
}
