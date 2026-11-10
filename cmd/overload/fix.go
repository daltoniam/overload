package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/checkout"
	"github.com/daltoniam/overload/postgres"
)

type fixOptions struct {
	checkout, base, workflow, fixer, test, until string
	rounds                                       int
	timeout                                      time.Duration
}

func parseFixOptions(args []string) (fixOptions, error) {
	var options fixOptions
	flags := flag.NewFlagSet("fix", flag.ContinueOnError)
	flags.StringVar(&options.checkout, "checkout", ".", "local git checkout")
	flags.StringVar(&options.base, "base", "main", "branch the changes are measured against")
	flags.StringVar(&options.workflow, "workflow", "", "saved PR workflow that reviews each round")
	flags.StringVar(&options.fixer, "fixer", "", "saved model that edits files (default: the workflow's main agent's model)")
	flags.StringVar(&options.test, "test", "", "command that runs the repository's tests, such as \"make test\"")
	flags.StringVar(&options.until, "until", "high", "fix findings at or above this severity: critical, high, medium or low")
	flags.IntVar(&options.rounds, "rounds", 3, "most fix rounds")
	flags.DurationVar(&options.timeout, "timeout", 4*time.Hour, "maximum time for the whole loop (up to 5h)")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 || options.workflow == "" {
		return options, errors.New("usage: overload fix --workflow NAME [--checkout DIR] [--base main] [--fixer MODEL] [--test CMD] [--rounds 3] [--until high] [--timeout 4h]")
	}
	if _, err := overload.SeverityRank(options.until); err != nil {
		return options, err
	}
	if options.timeout < time.Second || options.timeout > 5*time.Hour {
		return options, errors.New("fix timeout must be between 1 second and 5 hours")
	}
	if options.rounds < 1 || options.rounds > checkout.MaxFixRounds {
		return options, fmt.Errorf("rounds must be between 1 and %d", checkout.MaxFixRounds)
	}
	return options, nil
}

func manageFix(args []string) error {
	options, err := parseFixOptions(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	return runFix(ctx, store, options, os.Stdout, checkout.Fix)
}

// runFix resolves the workflow and fixer model (both under the model
// policy) and runs the loop.
func runFix(ctx context.Context, store *postgres.Store, options fixOptions, out io.Writer, fix func(context.Context, checkout.FixOptions) (checkout.FixResult, error)) error {
	workflow, err := store.ResolveRunnableWorkflow(ctx, options.workflow)
	if err != nil {
		return err
	}
	if workflow.Kind != "pr_review" {
		return fmt.Errorf("%s is not a PR review workflow", options.workflow)
	}
	fixer := workflow.Agents[0].Model
	if options.fixer != "" {
		settings, err := store.GetReviewSettings(ctx, options.fixer)
		if err != nil {
			return fmt.Errorf("fixer model %s: %w", options.fixer, err)
		}
		policy, err := store.LoadModelPolicy(ctx)
		if err != nil {
			return err
		}
		if err := policy.Check(settings.Catalog, settings.Model).Error(); err != nil {
			return fmt.Errorf("%w: %v", postgres.ErrModelPolicy, err)
		}
		fixer = settings.Profile()
	}
	result, err := fix(ctx, checkout.FixOptions{Dir: options.checkout, Base: options.base, Workflow: workflow, Fixer: fixer, TestCommand: options.test, Rounds: options.rounds, Until: options.until, Out: out})
	if result.Branch != "" {
		defer func() {
			_, _ = fmt.Fprintf(out, "Nothing was pushed. Inspect with: git log %s, or git -C %s diff %s. When done: git worktree remove %s\n", result.Branch, result.Worktree, options.base, result.Worktree)
		}()
	}
	if err != nil {
		return err
	}
	status := fmt.Sprintf("%d finding(s) remain", len(result.Remaining))
	if result.Clean {
		status = "the review is clean"
	}
	_, _ = fmt.Fprintf(out, "After %d fix round(s), %s.\n", result.Rounds, status)
	if result.TestsPassed != nil {
		_, _ = fmt.Fprintf(out, "Tests passed: %v\n", *result.TestsPassed)
	}
	return nil
}
