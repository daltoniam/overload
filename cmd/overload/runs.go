package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
)

const runsUsage = "usage: overload runs cost|transfer [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--repo owner/name] [--json]"

// manageRuns reports on runs: what they cost, or how long getting the
// repository to the reviewer took.
func manageRuns(args []string) error {
	if len(args) == 0 || (args[0] != "cost" && args[0] != "transfer") {
		return errors.New(runsUsage)
	}
	filter, asJSON, err := parseCostFlags(args[1:], time.Now())
	if err != nil {
		return err
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
	if args[0] == "transfer" {
		steps, err := store.TransferTimes(ctx, filter)
		if err != nil {
			return err
		}
		return writeTransfer(os.Stdout, steps, asJSON)
	}
	report, err := store.RunCosts(ctx, filter)
	if err != nil {
		return err
	}
	return writeCostReport(os.Stdout, report, asJSON)
}

func writeTransfer(out io.Writer, steps []postgres.TransferStep, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(steps)
	}
	for _, step := range steps {
		if _, err := fmt.Fprintf(out, "%-22s runs %-5d p50 %-10.1f p90 %-10.1f max %.1f\n", step.Step, step.Runs, step.P50, step.P90, step.Max); err != nil {
			return err
		}
	}
	return nil
}

// parseCostFlags reads the report window. --since defaults to the start of
// the current month (UTC); --until is exclusive and defaults to now.
func parseCostFlags(args []string, now time.Time) (postgres.CostFilter, bool, error) {
	flags := flag.NewFlagSet("runs cost", flag.ContinueOnError)
	since := flags.String("since", "", "first day to include (YYYY-MM-DD, UTC; default: the start of this month)")
	until := flags.String("until", "", "first day to leave out (YYYY-MM-DD, UTC; default: now)")
	repo := flags.String("repo", "", "only this repository (owner/name)")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return postgres.CostFilter{}, false, err
	}
	if flags.NArg() != 0 {
		return postgres.CostFilter{}, false, errors.New(runsUsage)
	}
	now = now.UTC()
	filter := postgres.CostFilter{Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), Until: now, Repository: *repo}
	for _, day := range []struct {
		text   string
		target *time.Time
	}{{*since, &filter.Since}, {*until, &filter.Until}} {
		if day.text == "" {
			continue
		}
		parsed, err := time.Parse("2006-01-02", day.text)
		if err != nil {
			return filter, false, fmt.Errorf("invalid date %q: use YYYY-MM-DD", day.text)
		}
		*day.target = parsed
	}
	if !filter.Until.After(filter.Since) {
		return filter, false, errors.New("--until must be after --since")
	}
	return filter, *asJSON, nil
}

func writeCostReport(out io.Writer, report postgres.CostReport, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	scope := "all repositories"
	if report.Repository != "" {
		scope = report.Repository
	}
	_, err := fmt.Fprintf(out, "%s, %s to %s (UTC)\nRuns with a recorded cost: %d\nTotal: %s\nAverage per run: %s\nMedian (p50): %s\np90: %s\n", scope, report.Since.Format("2006-01-02 15:04"), report.Until.Format("2006-01-02 15:04"), report.Runs, overload.FormatUSD(report.TotalUSD), overload.FormatUSD(report.AverageUSD), overload.FormatUSD(report.P50USD), overload.FormatUSD(report.P90USD))
	if err == nil && report.UnpricedRuns > 0 {
		_, err = fmt.Fprintf(out, "Not included: %s in %s to models without prices\n", overload.Plural(int(report.UnpricedCalls), "call"), overload.Plural(report.UnpricedRuns, "run"))
	}
	return err
}
