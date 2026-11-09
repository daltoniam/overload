package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/daltoniam/overload"
)

// Limits on what the fixer's tools read and run.
const (
	maxReadBytes   = 256 << 10
	maxWriteBytes  = 1 << 20
	maxTestOutput  = 16 << 10
	testTimeout    = 15 * time.Minute
	maxListEntries = 500
)

// localTool is a tool the fixer runs in this process.
type localTool struct {
	info     fantasy.ToolInfo
	run      func(context.Context, map[string]any) (string, error)
	provider fantasy.ProviderOptions
}

func (tool *localTool) Info() fantasy.ToolInfo                          { return tool.info }
func (tool *localTool) ProviderOptions() fantasy.ProviderOptions        { return tool.provider }
func (tool *localTool) SetProviderOptions(opts fantasy.ProviderOptions) { tool.provider = opts }

// Run returns failures to the model as error results so it can recover.
func (tool *localTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	arguments := map[string]any{}
	if strings.TrimSpace(call.Input) != "" {
		if err := json.Unmarshal([]byte(call.Input), &arguments); err != nil {
			return fantasy.NewTextErrorResponse("Tool input must be a JSON object: " + err.Error()), nil
		}
	}
	text, err := tool.run(ctx, arguments)
	if err != nil {
		return fantasy.NewTextErrorResponse(overload.TruncateUTF8(err.Error(), 4000)), nil
	}
	return fantasy.NewTextResponse(text), nil
}

func stringArgument(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name].(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return value, nil
}

// workPath cleans a model-supplied path relative to the worktree and
// refuses git's own files; os.Root refuses anything outside it.
func workPath(raw string) (string, error) {
	cleaned := path.Clean(strings.TrimPrefix(strings.ReplaceAll(raw, "\\", "/"), "./"))
	if cleaned == "" || cleaned == "." {
		return ".", nil
	}
	if !fs.ValidPath(cleaned) || cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
		return "", fmt.Errorf("%q is not a path inside the checkout", raw)
	}
	return cleaned, nil
}

// fixerTools gives a fixer read and write access to the worktree only, and
// lets it run the configured test command.
func fixerTools(root *os.Root, dir, testCommand string) []fantasy.AgentTool {
	stringProp := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	tools := []fantasy.AgentTool{
		&localTool{info: fantasy.ToolInfo{Name: "read_file", Description: "Read a file of the checkout.", Parameters: map[string]any{"path": stringProp("Path relative to the repository root")}, Required: []string{"path"}}, run: func(_ context.Context, arguments map[string]any) (string, error) {
			raw, err := stringArgument(arguments, "path")
			if err != nil {
				return "", err
			}
			name, err := workPath(raw)
			if err != nil {
				return "", err
			}
			data, err := root.ReadFile(name)
			if err != nil {
				return "", err
			}
			if len(data) > maxReadBytes {
				return string(data[:maxReadBytes]) + "\n[file truncated]", nil
			}
			return string(data), nil
		}},
		&localTool{info: fantasy.ToolInfo{Name: "write_file", Description: "Replace a file of the checkout with new content (create it if needed). Write the whole file.", Parameters: map[string]any{"path": stringProp("Path relative to the repository root"), "content": stringProp("The complete new content")}, Required: []string{"path", "content"}}, run: func(_ context.Context, arguments map[string]any) (string, error) {
			raw, err := stringArgument(arguments, "path")
			if err != nil {
				return "", err
			}
			content, err := stringArgument(arguments, "content")
			if err != nil {
				return "", err
			}
			name, err := workPath(raw)
			if err != nil || name == "." {
				return "", fmt.Errorf("%q is not a file inside the checkout", raw)
			}
			if len(content) > maxWriteBytes {
				return "", fmt.Errorf("content is larger than %d bytes", maxWriteBytes)
			}
			if dir := path.Dir(name); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return "", err
				}
			}
			if err := root.WriteFile(name, []byte(content), 0o644); err != nil {
				return "", err
			}
			return "Wrote " + name, nil
		}},
		&localTool{info: fantasy.ToolInfo{Name: "list_files", Description: "List a directory of the checkout.", Parameters: map[string]any{"path": stringProp("Directory relative to the repository root; empty for the root")}}, run: func(_ context.Context, arguments map[string]any) (string, error) {
			raw, _ := arguments["path"].(string)
			name, err := workPath(raw)
			if err != nil {
				return "", err
			}
			entries, err := fs.ReadDir(root.FS(), name)
			if err != nil {
				return "", err
			}
			var lines []string
			for _, entry := range entries {
				if entry.Name() == ".git" {
					continue
				}
				suffix := ""
				if entry.IsDir() {
					suffix = "/"
				}
				lines = append(lines, entry.Name()+suffix)
				if len(lines) == maxListEntries {
					lines = append(lines, "[more entries not shown]")
					break
				}
			}
			sort.Strings(lines)
			return strings.Join(lines, "\n"), nil
		}},
	}
	if testCommand != "" {
		tools = append(tools, &localTool{info: fantasy.ToolInfo{Name: "run_tests", Description: "Run the repository's tests (" + testCommand + ") and return the end of their output.", Parameters: map[string]any{}}, run: func(ctx context.Context, _ map[string]any) (string, error) {
			passed, output := RunTests(ctx, dir, testCommand)
			status := "Tests passed."
			if !passed {
				status = "Tests failed."
			}
			return status + "\n" + output, nil
		}})
	}
	return tools
}

// RunTests runs command with sh in dir and reports whether it passed, with
// the end of its output.
func RunTests(ctx context.Context, dir, command string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	process := exec.CommandContext(ctx, "sh", "-c", command)
	process.Dir = dir
	var output bytes.Buffer
	process.Stdout, process.Stderr = &output, &output
	err := process.Run()
	text := output.String()
	if len(text) > maxTestOutput {
		text = "[earlier output not shown]\n" + text[len(text)-maxTestOutput:]
	}
	if err != nil && ctx.Err() != nil {
		return false, text + "\n[stopped after " + testTimeout.String() + "]"
	}
	return err == nil, text
}
