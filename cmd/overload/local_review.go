package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/harness"
	"github.com/daltoniam/overload/postgres"
	reviewpkg "github.com/daltoniam/overload/review"
	"github.com/jackc/pgx/v5"
)

type reviewOptions struct {
	repo, workflow, profile, model, modelURL, promptProfile string
	pr, concurrency                                         int
	timeout                                                 time.Duration
}

func parseReviewOptions(args []string) (reviewOptions, error) {
	var options reviewOptions
	flags := flag.NewFlagSet("review", flag.ContinueOnError)
	flags.StringVar(&options.repo, "repo", "", "GitHub owner/name")
	flags.IntVar(&options.pr, "pr", 0, "pull request number")
	flags.StringVar(&options.workflow, "workflow", "", "saved PR workflow")
	flags.StringVar(&options.profile, "profile", "", "saved model (default model if omitted)")
	flags.StringVar(&options.model, "model", "", "model ID (overrides the saved model)")
	flags.StringVar(&options.modelURL, "model-url", "", "OpenAI-compatible base URL (overrides the saved model)")
	flags.StringVar(&options.promptProfile, "prompt-profile", "", "built-in prompt: context or switchboard-go")
	flags.DurationVar(&options.timeout, "timeout", 260*time.Minute, "maximum review duration (up to 5h)")
	flags.IntVar(&options.concurrency, "concurrency", 0, "changed files reviewed at once (overrides the saved model)")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	switch {
	case flags.NArg() != 0 || options.repo == "" || options.pr < 1:
		return options, errors.New("usage: overload review --repo owner/name --pr N [--workflow NAME | --profile NAME | --model MODEL --model-url URL]")
	case options.timeout < time.Second || options.timeout > 5*time.Hour:
		return options, errors.New("review timeout must be between 1 second and 5 hours")
	case options.concurrency < 0 || options.concurrency > overload.MaxReviewConcurrency:
		return options, fmt.Errorf("concurrency must be between 1 and %d", overload.MaxReviewConcurrency)
	case options.workflow != "" && (options.profile != "" || options.model != "" || options.modelURL != "" || options.promptProfile != ""):
		return options, errors.New("--workflow cannot be combined with model or prompt overrides")
	}
	return options, nil
}

// reviewWorkflow resolves what the review runs: a saved workflow, or a model
// (saved, default, or from flags and OVERLOAD_DEFAULT_MODEL*) with a
// built-in prompt.
func reviewWorkflow(ctx context.Context, store *postgres.Store, options reviewOptions) (overload.ResolvedWorkflow, error) {
	if options.workflow != "" {
		if store == nil {
			return overload.ResolvedWorkflow{}, errors.New("DATABASE_URL required for saved workflows")
		}
		return store.ResolveWorkflow(ctx, options.workflow)
	}
	profile := overload.ModelProfile{Provider: "openaicompat", BaseURL: os.Getenv("OVERLOAD_DEFAULT_MODEL_BASE_URL"), Model: os.Getenv("OVERLOAD_DEFAULT_MODEL"), APIKeyEnv: "OVERLOAD_DEFAULT_MODEL_API_KEY"}
	prompt := "context"
	if store != nil {
		settings, err := store.GetReviewSettings(ctx, options.profile)
		switch {
		case err == nil:
			profile, prompt = settings.Profile(), settings.PromptProfile
		case !errors.Is(err, pgx.ErrNoRows) || options.profile != "":
			return overload.ResolvedWorkflow{}, err
		}
	} else if options.profile != "" {
		return overload.ResolvedWorkflow{}, errors.New("DATABASE_URL required for saved models")
	}
	if options.model != "" {
		profile.Model = options.model
	}
	if options.modelURL != "" {
		profile.BaseURL = options.modelURL
	}
	if options.promptProfile != "" {
		prompt = options.promptProfile
	}
	return harness.ProfileWorkflow(profile, prompt)
}

func inlineReview(args []string) error {
	options, err := parseReviewOptions(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	token := github.TokenFromEnvironment()
	if token == "" {
		if token, err = github.TokenFromGH(ctx); err != nil {
			return err
		}
	}
	source, err := github.NewTokenClient(token)
	if err != nil {
		return err
	}
	var store *postgres.Store
	if os.Getenv("DATABASE_URL") != "" {
		if store, err = database(ctx); err != nil {
			return err
		}
		defer store.Pool.Close()
		if err := store.Migrate(ctx); err != nil {
			return err
		}
	}
	workflow, err := reviewWorkflow(ctx, store, options)
	if err != nil {
		return err
	}
	if options.concurrency > 0 {
		for index := range workflow.Agents {
			workflow.Agents[index].Model.Concurrency = options.concurrency
		}
	}
	var runID int64
	if store != nil {
		if runID, err = store.StartLocalReview(ctx, options.repo, options.pr, workflow); err != nil {
			return err
		}
	}
	spec, result, headSHA, reviewErr := (reviewpkg.LocalRunner{Source: source, Reviewer: harness.Reviewer{}, Workflow: workflow}).Review(ctx, options.repo, options.pr)
	if store != nil {
		finishCtx, finishCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer finishCancel()
		if err := store.FinishLocalReview(finishCtx, runID, spec, result, headSHA, reviewErr); err != nil {
			return fmt.Errorf("save run %d: %w (review error: %v)", runID, err, reviewErr)
		}
	}
	if reviewErr != nil {
		return reviewErr
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	fmt.Printf("PR %s#%d at %s: %d findings (dry run)\n%s\n", options.repo, options.pr, headSHA, len(result.Findings), result.Summary)
	if routing, ok := result.Metrics["routing"].(overload.Routing); ok {
		for _, agent := range routing.Agents {
			fmt.Printf("%s reviewed %d of %s, %s\n", agent.Agent, agent.Reviewed, plural(len(agent.Files), "file"), plural(agent.Findings, "finding"))
		}
		if len(routing.Skipped) > 0 {
			fmt.Printf("Skipped %s: %s\n", plural(len(routing.Skipped), "file"), strings.Join(routing.Skipped, ", "))
		}
		if routing.Planner != "" {
			fmt.Printf("Planner: %s\n", routing.Planner)
		}
		for _, note := range routing.Degraded {
			fmt.Printf("Partial review: %s\n", note)
		}
	}
	if len(result.Dropped) > 0 {
		fmt.Printf("Verifier dropped %s (saved on the run, not posted)\n", plural(len(result.Dropped), "finding"))
	}
	for _, finding := range result.Findings {
		fmt.Printf("%s:%d [%s] %s (%s): %s\n", finding.Path, finding.Line, finding.Severity, finding.Title, strings.Join(finding.Agents, ", "), finding.Body)
	}
	if runID != 0 {
		fmt.Printf("Run %d: /runs/%d\n", runID, runID)
	}
	return nil
}

func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
