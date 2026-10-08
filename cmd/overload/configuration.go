package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/harness"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

func manageConfiguration(resource string, args []string) error {
	if len(args) == 0 || len(args) > 4 {
		return errors.New("usage: overload RESOURCE list|show|apply [path], overload workflows preview NAME [files|-], overload schedules run NAME, or overload tools check|delete NAME")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	write := func(value any, err error) error {
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(value)
	}
	if args[0] == "list" && len(args) == 1 {
		switch resource {
		case "agents":
			return write(store.ListAgents(ctx))
		case "workflows":
			return write(store.ListWorkflows(ctx))
		case "bindings":
			return write(store.ListBindings(ctx))
		case "repositories":
			return write(store.ListRepositories(ctx))
		case "schedules":
			return write(store.ListSchedules(ctx))
		case "tools":
			return write(store.ListToolServers(ctx))
		}
	}
	if resource == "schedules" && args[0] == "run" && len(args) == 2 {
		client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
		if err != nil {
			return err
		}
		runID, err := store.RunScheduleNow(ctx, client, args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Printf("Queued run %d: /runs/%d (the overload server runs it)\n", runID, runID)
		return err
	}
	if resource == "tools" && args[0] == "delete" && len(args) == 2 {
		return store.DeleteToolServer(ctx, args[1])
	}
	if resource == "tools" && args[0] == "check" && len(args) == 2 {
		server, err := store.GetToolServer(ctx, args[1])
		if err != nil {
			return fmt.Errorf("tool server %q: %w", args[1], err)
		}
		names, err := harness.ListTools(ctx, server)
		if err != nil {
			return fmt.Errorf("tool server %s: %w", server.Name, err)
		}
		return write(map[string]any{"server": server.Name, "tools": names}, nil)
	}
	if args[0] == "show" {
		return showConfiguration(ctx, store, resource, args)
	}
	if args[0] == "preview" && resource == "workflows" && (len(args) == 2 || len(args) == 3) {
		files := io.Reader(os.Stdin)
		if len(args) == 3 && args[2] != "-" {
			file, err := os.Open(args[2])
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			files = file
		}
		return previewWorkflow(ctx, store, args[1], io.LimitReader(files, 256<<10), os.Stdout)
	}
	if args[0] != "apply" || len(args) != 2 {
		return errors.New("usage: overload RESOURCE list|show|apply [path]")
	}
	if args[1] == "-" {
		return applyConfiguration(ctx, store, resource, os.Stdin)
	}
	file, err := os.Open(args[1])
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return applyConfiguration(ctx, store, resource, file)
}

func showConfiguration(ctx context.Context, store interface {
	ResolveWorkflow(context.Context, string) (overload.ResolvedWorkflow, error)
}, resource string, args []string) error {
	if resource != "workflows" || len(args) != 2 {
		return errors.New("usage: overload workflows show NAME")
	}
	workflow, err := store.ResolveWorkflow(ctx, args[1])
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(workflow)
}

type configurationStore interface {
	SaveAgent(context.Context, overload.AgentDefinition) error
	SaveWorkflow(context.Context, overload.Workflow) error
	SaveBinding(context.Context, overload.TriggerBinding) error
	SaveRepository(context.Context, string, bool, bool) error
	SaveSchedule(context.Context, overload.Schedule) error
	SaveToolServer(context.Context, overload.ToolServer) error
}

// maxConfigurationInput bounds a configuration file. A workflow with a
// planner, a verifier and eight scoped sub-agents fits easily.
const maxConfigurationInput = 1 << 20

// readConfiguration reads one configuration document, reporting input over
// maxConfigurationInput as an error instead of silently cutting it.
func readConfiguration(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxConfigurationInput+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigurationInput {
		return nil, fmt.Errorf("configuration input is larger than %d bytes", maxConfigurationInput)
	}
	return data, nil
}

func applyConfiguration(ctx context.Context, store configurationStore, resource string, reader io.Reader) error {
	data, err := readConfiguration(reader)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decode := func(label string, target any) error {
		if err := decoder.Decode(target); err != nil {
			return fmt.Errorf("invalid %s JSON: %w", label, err)
		}
		if decoder.More() {
			return fmt.Errorf("invalid %s JSON: the input must hold exactly one object", label)
		}
		return nil
	}
	var result any
	switch resource {
	case "agents":
		var agent overload.AgentDefinition
		if err := decode("agent", &agent); err != nil {
			if strings.Contains(err.Error(), `"entry_prompt"`) || strings.Contains(err.Error(), `"review_prompt"`) {
				return errors.New(`invalid agent JSON: agents now hold their prompt text in "prompt" instead of naming a prompt`)
			}
			return err
		}
		if err := store.SaveAgent(ctx, agent); err != nil {
			return err
		}
		result = agent
	case "workflows":
		var workflow overload.Workflow
		if err := decode("workflow", &workflow); err != nil {
			return err
		}
		if err := store.SaveWorkflow(ctx, workflow); err != nil {
			return err
		}
		result = workflow
	case "bindings":
		var binding overload.TriggerBinding
		if err := decode("binding", &binding); err != nil {
			return err
		}
		if err := store.SaveBinding(ctx, binding); err != nil {
			return err
		}
		result = binding
	case "schedules":
		var schedule overload.Schedule
		if err := decode("schedule", &schedule); err != nil {
			return err
		}
		if err := store.SaveSchedule(ctx, schedule); err != nil {
			return err
		}
		result = schedule
	case "tools":
		var server overload.ToolServer
		if err := decode("tool server", &server); err != nil {
			return err
		}
		if err := store.SaveToolServer(ctx, server); err != nil {
			return err
		}
		result = server
	case "repositories":
		var repo struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			DryRun  bool   `json:"dry_run"`
		}
		if err := decode("repository", &repo); err != nil {
			return err
		}
		if err := store.SaveRepository(ctx, repo.Name, repo.Enabled, repo.DryRun); err != nil {
			return err
		}
		result = repo
	default:
		return fmt.Errorf("unsupported resource %q", resource)
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

// previewWorkflow prints which agent of a saved PR workflow would review
// each changed file listed in files (one per line), without calling a model.
func previewWorkflow(ctx context.Context, store interface {
	ResolveWorkflow(context.Context, string) (overload.ResolvedWorkflow, error)
}, name string, files io.Reader, out io.Writer) error {
	workflow, err := store.ResolveWorkflow(ctx, name)
	if err != nil {
		return err
	}
	if workflow.Kind != "pr_review" {
		return errors.New("only PR review workflows route changed files")
	}
	text, err := io.ReadAll(files)
	if err != nil {
		return err
	}
	paths, err := overload.ParseChangedFiles(string(text))
	if err != nil {
		return err
	}
	preview, _ := workflow.Preview(paths)
	return json.NewEncoder(out).Encode(preview)
}
