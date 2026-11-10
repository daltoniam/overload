package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/templates/pages"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

const bakeoffUsage = `usage: overload bakeoff COMMAND
  case --repo DIR --commit SHA [--name NAME]     print a case built from a commit that fixed a bug
  suite save NAME [--description TEXT] CASE.json...
  suite list | suite delete NAME
  run --models NAME,NAME [--suites planted,NAME] [--runs 3]
  show ID`

func manageBakeoff(args []string) error {
	if len(args) == 0 {
		return errors.New(bakeoffUsage)
	}
	if args[0] == "case" {
		return bakeoffCase(context.Background(), args[1:], os.Stdout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	return runBakeoff(ctx, store, args, os.Stdout)
}

func bakeoffCase(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("bakeoff case", flag.ContinueOnError)
	repo := flags.String("repo", ".", "local git checkout")
	commit := flags.String("commit", "", "the commit that fixed the bug")
	name := flags.String("name", "", "case name (default commit-<hash>)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *commit == "" {
		return errors.New(bakeoffUsage)
	}
	item, err := eval.CaseFromCommit(ctx, *repo, *commit, *name)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(item)
}

// bakeoffStore is what the bake-off commands need from the store.
type bakeoffStore interface {
	SaveSuite(context.Context, eval.Suite) error
	ListSuites(context.Context) ([]postgres.SuiteSummary, error)
	DeleteSuite(context.Context, string) error
	GetBakeoff(context.Context, int64) (postgres.Bakeoff, error)
	queueBakeoff(ctx context.Context, models, suites []string, runs int) (int64, error)
}

type cliBakeoffStore struct{ *postgres.Store }

func (store cliBakeoffStore) queueBakeoff(ctx context.Context, models, suites []string, runs int) (int64, error) {
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		return 0, err
	}
	return store.CreateBakeoff(ctx, client, models, suites, runs)
}

func runBakeoff(ctx context.Context, store *postgres.Store, args []string, out io.Writer) error {
	return runBakeoffWith(ctx, cliBakeoffStore{store}, args, out)
}

func runBakeoffWith(ctx context.Context, store bakeoffStore, args []string, out io.Writer) error {
	switch {
	case len(args) >= 2 && args[0] == "suite" && args[1] == "save":
		flags := flag.NewFlagSet("bakeoff suite save", flag.ContinueOnError)
		description := flags.String("description", "", "what the suite covers")
		if len(args) < 3 {
			return errors.New(bakeoffUsage)
		}
		if err := flags.Parse(args[3:]); err != nil {
			return err
		}
		suite := eval.Suite{Name: args[2], Description: *description}
		for _, path := range flags.Args() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var item eval.Case
			if err := json.Unmarshal(data, &item); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			suite.Cases = append(suite.Cases, item)
		}
		if err := store.SaveSuite(ctx, suite); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "Saved suite %s with %d case(s)\n", suite.Name, len(suite.Cases))
		return err
	case len(args) == 2 && args[0] == "suite" && args[1] == "list":
		suites, err := store.ListSuites(ctx)
		if err != nil {
			return err
		}
		writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "SUITE\tCASES\tDESCRIPTION")
		for _, suite := range suites {
			_, _ = fmt.Fprintf(writer, "%s\t%d\t%s\n", suite.Name, suite.Cases, suite.Description)
		}
		return writer.Flush()
	case len(args) == 3 && args[0] == "suite" && args[1] == "delete":
		return store.DeleteSuite(ctx, args[2])
	case len(args) >= 1 && args[0] == "run":
		flags := flag.NewFlagSet("bakeoff run", flag.ContinueOnError)
		models := flags.String("models", "", "comma-separated model connection names")
		suites := flags.String("suites", eval.BuiltinSuite, "comma-separated suite names")
		runs := flags.Int("runs", 3, "runs per model and case")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *models == "" {
			return errors.New(bakeoffUsage)
		}
		id, err := store.queueBakeoff(ctx, splitNames(*models), splitNames(*suites), *runs)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Queued bake-off %d: /bakeoffs/%d (the overload server runs it; overload bakeoff show %d)\n", id, id, id)
		return err
	case len(args) == 2 && args[0] == "show":
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return errors.New(bakeoffUsage)
		}
		bakeoff, err := store.GetBakeoff(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no bake-off %d", id)
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "Bake-off %d: %s, %d result(s)\n", bakeoff.ID, bakeoff.Status, len(bakeoff.Results))
		writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "MODEL\tFOUND\tMISPLACED\tOTHER\tTIME/REVIEW\tFAILED")
		for _, summary := range pages.Summarize(bakeoff) {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%d of %d\n", summary.Model, summary.Found(), summary.MisplacedCount(), summary.ExtraCount(), summary.AverageTime(), summary.Failed, summary.Reviews)
		}
		return writer.Flush()
	default:
		return errors.New(bakeoffUsage)
	}
}

// splitNames splits a comma-separated list, trimming spaces and dropping
// empty and repeated names.
func splitNames(list string) []string {
	var names []string
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}
