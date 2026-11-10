package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
)

const budgetUsage = "usage: overload budget show | overload budget set [--repo owner/name|connection:path] [--monthly USD|none] [--daily USD|none] [--skip-status success|error]"

// manageBudget shows and sets spending caps.
func manageBudget(args []string) error {
	if len(args) == 0 {
		return errors.New(budgetUsage)
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
	switch args[0] {
	case "show":
		budgets, err := store.Budgets(ctx)
		if err != nil {
			return err
		}
		status, err := store.GlobalBudgetStatus(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"budgets": budgets, "month_spent_usd": status.MonthSpent, "today_spent_usd": status.DaySpent})
	case "set":
		budgets, err := store.Budgets(ctx)
		if err != nil {
			return err
		}
		budget, err := parseBudgetFlags(args[1:], budgets)
		if err != nil {
			return err
		}
		if err := store.SaveBudget(ctx, budget); err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, "Budget saved")
		return err
	}
	return errors.New(budgetUsage)
}

// parseBudgetFlags applies the flags given to the current budget of the
// chosen scope; flags left out keep their values, and "none" clears a cap.
func parseBudgetFlags(args []string, current []postgres.Budget) (postgres.Budget, error) {
	flags := flag.NewFlagSet("budget set", flag.ContinueOnError)
	repo := flags.String("repo", "", "set this repository's budget instead of the global one: owner/name on GitHub, connection:path on GitLab or Forgejo")
	monthly := flags.String("monthly", "", "monthly cap in US dollars, or none")
	daily := flags.String("daily", "", "daily cap in US dollars, or none")
	skipStatus := flags.String("skip-status", "", "commit status for reviews skipped for budget: success (default; does not block merges) or error")
	if err := flags.Parse(args); err != nil {
		return postgres.Budget{}, err
	}
	if flags.NArg() != 0 || flags.NFlag() == 0 {
		return postgres.Budget{}, errors.New(budgetUsage)
	}
	budget := postgres.Budget{Repository: *repo, SkipStatus: postgres.BudgetSkipSuccess}
	for _, existing := range current {
		if existing.Repository == *repo {
			budget = existing
			budget.FromEnvironment = false
		}
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, field := range []struct {
		name, text string
		target     **float64
	}{{"monthly", *monthly, &budget.MonthlyUSD}, {"daily", *daily, &budget.DailyUSD}} {
		if !set[field.name] {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(field.text), "none") {
			*field.target = nil
			continue
		}
		value, err := overload.ParsePrice(field.text)
		if err != nil || value == nil {
			return budget, fmt.Errorf("--%s must be a dollar amount or none", field.name)
		}
		*field.target = value
	}
	if set["skip-status"] {
		budget.SkipStatus = *skipStatus
	}
	return budget, nil
}
