package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/harness"
)

// starterStore is what creating the starter cohort needs.
type starterStore interface {
	ListReviewSettings(context.Context) ([]overload.ReviewSettings, error)
	ListAgents(context.Context) ([]overload.AgentDefinition, error)
	ListWorkflows(context.Context) ([]overload.Workflow, error)
	SaveAgent(context.Context, overload.AgentDefinition) error
	SaveWorkflow(context.Context, overload.Workflow) error
}

// createStarterCohort saves the starter cohort's agents and workflow, all
// on one model connection. It refuses to replace agents or a workflow
// that already exist unless --force is given, and checks everything before
// saving anything.
func createStarterCohort(ctx context.Context, store starterStore, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("workflows starter", flag.ContinueOnError)
	model := flags.String("model", "", "saved model connection every agent uses")
	name := flags.String("name", "review-cohort", "workflow name; agents are named <name>-<role>")
	budget := flags.Int("max-review-tokens", 0, "token budget per review (0 for none)")
	force := flags.Bool("force", false, "replace agents and a workflow with the same names")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *model == "" {
		return errors.New("usage: overload workflows starter --model NAME [--name review-cohort] [--max-review-tokens N] [--force]")
	}
	settings, err := store.ListReviewSettings(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(settings, func(setting overload.ReviewSettings) bool { return setting.Name == *model }) {
		return fmt.Errorf("no model named %s; add one with overload settings set", *model)
	}
	cohort := harness.StarterCohort()
	workflow := overload.Workflow{Name: *name, Kind: "pr_review", Enabled: true, Scopes: map[string]overload.Scope{}, SkipPaths: cohort.SkipPaths, SensitivePaths: cohort.SensitivePaths, VerifierPrompt: cohort.VerifierPrompt, MaxFindings: cohort.MaxFindings, MaxReviewTokens: *budget}
	var agents []overload.AgentDefinition
	for index, agent := range cohort.Agents {
		definition := overload.AgentDefinition{Name: *name + "-" + agent.Role, Kind: "pr_review", Model: *model, Prompt: agent.Prompt, Enabled: true}
		if err := definition.Validate(); err != nil {
			return fmt.Errorf("agent %s: %w", definition.Name, err)
		}
		agents = append(agents, definition)
		workflow.Agents = append(workflow.Agents, definition.Name)
		if index > 0 {
			workflow.Scopes[definition.Name] = agent.Scope
		}
	}
	workflow.Normalize()
	if err := workflow.Validate(); err != nil {
		return err
	}
	if !*force {
		if existing, err := existingStarterNames(ctx, store, workflow); err != nil {
			return err
		} else if len(existing) > 0 {
			return fmt.Errorf("%s already exist; choose another --name or pass --force to replace them", strings.Join(existing, ", "))
		}
	}
	for _, agent := range agents {
		if err := store.SaveAgent(ctx, agent); err != nil {
			return fmt.Errorf("save agent %s: %w", agent.Name, err)
		}
	}
	if err := store.SaveWorkflow(ctx, workflow); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Saved workflow %s with %d agents on %s. Bind it to a repository, or run overload review --checkout . --workflow %s\n", workflow.Name, len(workflow.Agents), *model, workflow.Name)
	return err
}

// existingStarterNames lists the workflow and agents of workflow that are
// already saved.
func existingStarterNames(ctx context.Context, store starterStore, workflow overload.Workflow) ([]string, error) {
	var existing []string
	workflows, err := store.ListWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(workflows, func(saved overload.Workflow) bool { return saved.Name == workflow.Name }) {
		existing = append(existing, "workflow "+workflow.Name)
	}
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	for _, saved := range agents {
		if slices.Contains(workflow.Agents, saved.Name) {
			existing = append(existing, "agent "+saved.Name)
		}
	}
	return existing, nil
}
