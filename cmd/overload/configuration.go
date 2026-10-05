package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/daltoniam/overload"
)

func manageConfiguration(resource string, args []string) error {
	if len(args) == 0 || len(args) > 4 {
		return errors.New("usage: overload RESOURCE list|show|apply [path], or overload workflows preview NAME [files|-]")
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
		case "prompts":
			return write(store.ListPrompts(ctx))
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
		}
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
		return applyConfiguration(ctx, store, resource, io.LimitReader(os.Stdin, 64<<10))
	}
	file, err := os.Open(args[1])
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return applyConfiguration(ctx, store, resource, io.LimitReader(file, 64<<10))
}

func showConfiguration(ctx context.Context, store interface {
	ResolveWorkflow(context.Context, string) (overload.ResolvedWorkflow, error)
	GetPrompt(context.Context, string, string, int) (overload.PromptTemplate, error)
}, resource string, args []string) error {
	if resource == "workflows" && len(args) == 2 {
		workflow, err := store.ResolveWorkflow(ctx, args[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(workflow)
	}
	if resource != "prompts" || (len(args) != 3 && len(args) != 4) {
		return errors.New("usage: overload prompts show KIND NAME [REVISION]")
	}
	revision := 0
	if len(args) == 4 {
		var err error
		revision, err = strconv.Atoi(args[3])
		if err != nil || revision < 1 {
			return errors.New("invalid prompt revision")
		}
	}
	prompt, err := store.GetPrompt(ctx, args[1], args[2], revision)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(prompt)
}

type configurationStore interface {
	SavePrompt(context.Context, overload.PromptTemplate) (overload.PromptTemplate, error)
	SaveAgent(context.Context, overload.AgentDefinition) error
	SaveWorkflow(context.Context, overload.Workflow) error
	SaveBinding(context.Context, overload.TriggerBinding) error
	SaveRepository(context.Context, string, bool, bool) error
	SaveSchedule(context.Context, overload.Schedule) error
}

func applyConfiguration(ctx context.Context, store configurationStore, resource string, reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var result any
	switch resource {
	case "prompts":
		var prompt overload.PromptTemplate
		if err := decoder.Decode(&prompt); err != nil {
			return errors.New("invalid prompt JSON")
		}
		stored, err := store.SavePrompt(ctx, prompt)
		result = stored
		if err != nil {
			return err
		}
	case "agents":
		var agent overload.AgentDefinition
		if err := decoder.Decode(&agent); err != nil {
			return errors.New("invalid agent JSON")
		}
		if err := store.SaveAgent(ctx, agent); err != nil {
			return err
		}
		result = agent
	case "workflows":
		var workflow overload.Workflow
		if err := decoder.Decode(&workflow); err != nil {
			return errors.New("invalid workflow JSON")
		}
		if err := store.SaveWorkflow(ctx, workflow); err != nil {
			return err
		}
		result = workflow
	case "bindings":
		var binding overload.TriggerBinding
		if err := decoder.Decode(&binding); err != nil {
			return errors.New("invalid binding JSON")
		}
		if err := store.SaveBinding(ctx, binding); err != nil {
			return err
		}
		result = binding
	case "schedules":
		var schedule overload.Schedule
		if err := decoder.Decode(&schedule); err != nil {
			return errors.New("invalid schedule JSON")
		}
		if err := store.SaveSchedule(ctx, schedule); err != nil {
			return err
		}
		result = schedule
	case "repositories":
		var repo struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			DryRun  bool   `json:"dry_run"`
		}
		if err := decoder.Decode(&repo); err != nil {
			return errors.New("invalid repository JSON")
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
