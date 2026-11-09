package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/harness"
)

// starterStore is what creating the starter cohort needs.
type starterStore interface {
	ListReviewSettings(context.Context) ([]overload.ReviewSettings, error)
	SaveAgent(context.Context, overload.AgentDefinition) error
	SaveWorkflow(context.Context, overload.Workflow) error
}

// createStarterCohort saves the starter cohort's agents and workflow, all
// on one model connection, and returns the workflow's name.
func createStarterCohort(ctx context.Context, store starterStore, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("workflows starter", flag.ContinueOnError)
	model := flags.String("model", "", "saved model connection every agent uses")
	name := flags.String("name", "review-cohort", "workflow name; agents are named <name>-<role>")
	budget := flags.Int("max-review-tokens", 0, "token budget per review (0 for none)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *model == "" {
		return errors.New("usage: overload workflows starter --model NAME [--name review-cohort] [--max-review-tokens N]")
	}
	settings, err := store.ListReviewSettings(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, setting := range settings {
		found = found || setting.Name == *model
	}
	if !found {
		return fmt.Errorf("no model named %s; add one with overload settings set", *model)
	}
	cohort := harness.StarterCohort()
	workflow := overload.Workflow{Name: *name, Kind: "pr_review", Enabled: true, Scopes: map[string]overload.Scope{}, SkipPaths: cohort.SkipPaths, SensitivePaths: cohort.SensitivePaths, VerifierPrompt: cohort.VerifierPrompt, MaxFindings: cohort.MaxFindings, MaxReviewTokens: *budget}
	for index, agent := range cohort.Agents {
		agentName := *name + "-" + agent.Role
		if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: agentName, Kind: "pr_review", Model: *model, Prompt: agent.Prompt, Enabled: true}); err != nil {
			return fmt.Errorf("save agent %s: %w", agentName, err)
		}
		workflow.Agents = append(workflow.Agents, agentName)
		if index > 0 {
			workflow.Scopes[agentName] = agent.Scope
		}
	}
	workflow.Normalize()
	if err := workflow.Validate(); err != nil {
		return err
	}
	if err := store.SaveWorkflow(ctx, workflow); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Saved workflow %s with %d agents on %s. Bind it to a repository, or run overload review --checkout . --workflow %s\n", workflow.Name, len(workflow.Agents), *model, workflow.Name)
	return err
}
