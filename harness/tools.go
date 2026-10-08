package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/daltoniam/overload"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool call limits. Results are cut so one large response cannot fill the
// model's context; each call has its own time limit.
const (
	maxToolOutputBytes = 32 << 10
	maxToolDescription = 8000
	toolCallTimeout    = 5 * time.Minute
	recordedInputBytes = 4 << 10
	recordedOutputMax  = 8 << 10
)

// SwitchboardSessionHeader keeps Switchboard's per-session state (pinned
// results, context) together for the calls of one run.
const SwitchboardSessionHeader = "X-Switchboard-Session-Id"

// ToolEvent is one tool call, for the run timeline.
type ToolEvent struct {
	Server   string
	Tool     string
	Input    string
	Output   string
	IsError  bool
	Duration time.Duration
}

// Toolset is an agent's open connections to its tool servers for one run.
type Toolset struct {
	sessions []*mcp.ClientSession
	tools    []fantasy.AgentTool
	calls    atomic.Int64
	maxCalls int64
	record   func(ToolEvent)
}

var toolNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ConnectTools opens an MCP session to each server and lists its tools.
// Model-facing names are "<server>_<tool>". sessionID scopes Switchboard
// session state to the run; maxCalls caps the run's tool calls.
func ConnectTools(ctx context.Context, servers []overload.ToolServer, sessionID string, maxCalls int, record func(ToolEvent)) (*Toolset, error) {
	set := &Toolset{maxCalls: int64(maxCalls), record: record}
	names := map[string]string{}
	for _, server := range servers {
		session, err := connectServer(ctx, server, sessionID)
		if err != nil {
			_ = set.Close()
			return nil, fmt.Errorf("tool server %s: %w", server.Name, err)
		}
		set.sessions = append(set.sessions, session)
		for tool, err := range session.Tools(ctx, nil) {
			if err != nil {
				_ = set.Close()
				return nil, fmt.Errorf("tool server %s: list tools: %w", server.Name, err)
			}
			name := modelToolName(server.Name, tool.Name)
			if previous, taken := names[name]; taken {
				_ = set.Close()
				return nil, fmt.Errorf("tool name %s is used by both %s and %s", name, previous, server.Name+"/"+tool.Name)
			}
			names[name] = server.Name + "/" + tool.Name
			info, err := toolInfo(name, server, tool)
			if err != nil {
				_ = set.Close()
				return nil, fmt.Errorf("tool server %s: tool %s: %w", server.Name, tool.Name, err)
			}
			set.tools = append(set.tools, &mcpTool{set: set, session: session, server: server.Name, remote: tool.Name, info: info})
		}
	}
	if len(set.tools) == 0 && len(servers) > 0 {
		_ = set.Close()
		return nil, errors.New("tool servers offer no tools")
	}
	return set, nil
}

// Tools returns the model-facing tools.
func (set *Toolset) Tools() []fantasy.AgentTool { return set.tools }

// Calls reports how many tool calls the run made.
func (set *Toolset) Calls() int { return int(set.calls.Load()) }

func (set *Toolset) Close() error {
	var errs []error
	for _, session := range set.sessions {
		errs = append(errs, session.Close())
	}
	return errors.Join(errs...)
}

// ListTools connects to one server and returns its tool names, to check a
// server's address and token from the UI or CLI.
func ListTools(ctx context.Context, server overload.ToolServer) ([]string, error) {
	session, err := connectServer(ctx, server, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		names = append(names, tool.Name)
	}
	return names, nil
}

func connectServer(ctx context.Context, server overload.ToolServer, sessionID string) (*mcp.ClientSession, error) {
	if err := server.Validate(); err != nil {
		return nil, err
	}
	headers := http.Header{}
	if server.TokenEnv != "" {
		token := os.Getenv(server.TokenEnv)
		if token == "" {
			return nil, fmt.Errorf("token variable %s is not set", server.TokenEnv)
		}
		headers.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		headers.Set(SwitchboardSessionHeader, sessionID)
	}
	headers.Set("User-Agent", "overload")
	transport := &mcp.StreamableClientTransport{
		Endpoint:             server.URL,
		HTTPClient:           &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: headers}, Timeout: toolCallTimeout + 30*time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           2,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "overload", Version: "1"}, nil)
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return client.Connect(connectCtx, transport, nil)
}

type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (transport headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	for name, values := range transport.headers {
		clone.Header[name] = values
	}
	return transport.base.RoundTrip(clone)
}

func modelToolName(server, tool string) string {
	name := toolNameUnsafe.ReplaceAllString(server+"_"+tool, "_")
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func toolInfo(name string, server overload.ToolServer, tool *mcp.Tool) (fantasy.ToolInfo, error) {
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if tool.InputSchema != nil {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return fantasy.ToolInfo{}, err
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			return fantasy.ToolInfo{}, fmt.Errorf("unsupported input schema: %w", err)
		}
	}
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}
	if schema.Required == nil {
		schema.Required = []string{}
	}
	description := tool.Description
	if server.Description != "" {
		description = server.Description + "\n\n" + description
	}
	return fantasy.ToolInfo{Name: name, Description: overload.TruncateUTF8(description, maxToolDescription), Parameters: schema.Properties, Required: schema.Required}, nil
}

type mcpTool struct {
	set      *Toolset
	session  *mcp.ClientSession
	server   string
	remote   string
	info     fantasy.ToolInfo
	provider fantasy.ProviderOptions
}

func (tool *mcpTool) Info() fantasy.ToolInfo                          { return tool.info }
func (tool *mcpTool) ProviderOptions() fantasy.ProviderOptions        { return tool.provider }
func (tool *mcpTool) SetProviderOptions(opts fantasy.ProviderOptions) { tool.provider = opts }

// Run calls the remote tool. Failures are returned to the model as error
// results so it can recover; only the run's own deadline stops the agent.
func (tool *mcpTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	started := time.Now()
	event := ToolEvent{Server: tool.server, Tool: tool.remote, Input: overload.TruncateUTF8(call.Input, recordedInputBytes)}
	response := tool.call(ctx, call)
	event.Output, event.IsError, event.Duration = overload.TruncateUTF8(response.Content, recordedOutputMax), response.IsError, time.Since(started)
	if tool.set.record != nil {
		tool.set.record(event)
	}
	return response, nil
}

func (tool *mcpTool) call(ctx context.Context, call fantasy.ToolCall) fantasy.ToolResponse {
	if tool.set.calls.Add(1) > tool.set.maxCalls {
		return fantasy.NewTextErrorResponse("Tool call limit reached for this run. Stop calling tools and write your final answer now.")
	}
	arguments := map[string]any{}
	if strings.TrimSpace(call.Input) != "" {
		if err := json.Unmarshal([]byte(call.Input), &arguments); err != nil {
			return fantasy.NewTextErrorResponse("Tool input must be a JSON object: " + err.Error())
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	result, err := tool.session.CallTool(callCtx, &mcp.CallToolParams{Name: tool.remote, Arguments: arguments})
	if err != nil {
		return fantasy.NewTextErrorResponse("Tool call failed: " + overload.TruncateUTF8(err.Error(), 2000))
	}
	text := toolResultText(result)
	if result.IsError {
		return fantasy.NewTextErrorResponse(text)
	}
	return fantasy.NewTextResponse(text)
}

func toolResultText(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		switch typed := content.(type) {
		case *mcp.TextContent:
			parts = append(parts, typed.Text)
		case *mcp.ImageContent:
			parts = append(parts, "[image "+typed.MIMEType+" omitted]")
		case *mcp.AudioContent:
			parts = append(parts, "[audio "+typed.MIMEType+" omitted]")
		case *mcp.ResourceLink:
			parts = append(parts, "[resource "+typed.URI+"]")
		case *mcp.EmbeddedResource:
			if typed.Resource != nil && typed.Resource.Text != "" {
				parts = append(parts, typed.Resource.Text)
			} else if typed.Resource != nil {
				parts = append(parts, "[resource "+typed.Resource.URI+"]")
			}
		}
	}
	if len(parts) == 0 && result.StructuredContent != nil {
		if raw, err := json.Marshal(result.StructuredContent); err == nil {
			parts = append(parts, string(raw))
		}
	}
	text := strings.Join(parts, "\n")
	if len(text) > maxToolOutputBytes {
		text = overload.TruncateUTF8(text, maxToolOutputBytes) + fmt.Sprintf("\n[output truncated: %d of %d bytes shown; narrow the request for the rest]", maxToolOutputBytes, len(text))
	}
	return text
}
