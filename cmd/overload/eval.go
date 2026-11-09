package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/queue"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

const evalUsage = "usage: overload eval prs --repo owner/name --prs 101,102,110-115 --workflows a,b --out results.json [--labels labels.json] [--csv summary.csv] [--concurrency N] [--timeout 6h]"

type evalOptions struct {
	repo        string
	prs         []int
	workflows   []string
	labels      string
	out         string
	csv         string
	concurrency int
	timeout     time.Duration
}

func parseEvalOptions(args []string) (evalOptions, error) {
	var options evalOptions
	if len(args) == 0 || args[0] != "prs" {
		return options, errors.New(evalUsage)
	}
	flags := flag.NewFlagSet("eval prs", flag.ContinueOnError)
	repo := flags.String("repo", "", "repository (owner/name) overload already knows")
	prs := flags.String("prs", "", "pull request numbers: 101,102 or ranges such as 110-115")
	workflows := flags.String("workflows", "", "PR workflows to compare, comma separated")
	flags.StringVar(&options.labels, "labels", "", "optional JSON file of known bugs: [{\"pr\":101,\"path\":\"a.go\",\"line\":12,\"window\":3,\"keywords\":[\"nil\"]}]")
	flags.StringVar(&options.out, "out", "", "JSON results file")
	flags.StringVar(&options.csv, "csv", "", "CSV summary file (default: the --out path with .csv)")
	flags.IntVar(&options.concurrency, "concurrency", 2, "runs of this evaluation queued or running at once")
	flags.DurationVar(&options.timeout, "timeout", 6*time.Hour, "give up after this long")
	if err := flags.Parse(args[1:]); err != nil {
		return options, err
	}
	options.repo = *repo
	if flags.NArg() != 0 || options.repo == "" || *prs == "" || *workflows == "" || options.out == "" {
		return options, errors.New(evalUsage)
	}
	numbers, err := parsePRList(*prs)
	if err != nil {
		return options, err
	}
	options.prs = numbers
	seen := map[string]bool{}
	for _, name := range strings.Split(*workflows, ",") {
		if name = strings.TrimSpace(name); name != "" && !seen[name] {
			seen[name] = true
			options.workflows = append(options.workflows, name)
		}
	}
	switch {
	case len(options.workflows) == 0:
		return options, errors.New("--workflows names no workflow")
	case options.concurrency < 1 || options.concurrency > 64:
		return options, errors.New("--concurrency must be between 1 and 64")
	case options.timeout <= 0:
		return options, errors.New("--timeout must be positive")
	}
	if options.csv == "" {
		options.csv = strings.TrimSuffix(options.out, ".json") + ".csv"
	}
	if samePath(options.out, options.csv) {
		return options, fmt.Errorf("the JSON results (--out) and CSV summary (--csv) need different files: both are %s", options.csv)
	}
	return options, nil
}

// samePath reports whether two output paths name the same file.
func samePath(first, second string) bool {
	firstAbs, err1 := filepath.Abs(first)
	secondAbs, err2 := filepath.Abs(second)
	if err1 != nil || err2 != nil {
		return filepath.Clean(first) == filepath.Clean(second)
	}
	if firstAbs == secondAbs {
		return true
	}
	firstInfo, err1 := os.Stat(firstAbs)
	secondInfo, err2 := os.Stat(secondAbs)
	return err1 == nil && err2 == nil && os.SameFile(firstInfo, secondInfo)
}

// parsePRList reads "101,102,110-115" in order without repeats.
func parsePRList(text string) ([]int, error) {
	var numbers []int
	seen := map[int]bool{}
	add := func(number int) {
		if !seen[number] {
			seen[number] = true
			numbers = append(numbers, number)
		}
	}
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "#"))
		if part == "" {
			continue
		}
		low, high, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(low)
		if err != nil || first < 1 {
			return nil, fmt.Errorf("invalid pull request number %q", part)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(high); err != nil || last < first || last-first > 1000 {
				return nil, fmt.Errorf("invalid pull request range %q", part)
			}
		}
		for number := first; ; number++ {
			add(number)
			if number == last {
				break
			}
		}
	}
	if len(numbers) == 0 || len(numbers) > 5000 {
		return nil, errors.New("--prs must list between 1 and 5000 pull requests")
	}
	return numbers, nil
}

func readLabels(path string) ([]eval.Label, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var labels []eval.Label
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&labels); err != nil {
		return nil, fmt.Errorf("invalid labels file: %w", err)
	}
	for index, label := range labels {
		if label.PR < 1 || label.Path == "" || label.Line < 1 || label.Window < 0 {
			return nil, fmt.Errorf("label %d needs pr, path, a positive line and a window of 0 or more", index+1)
		}
	}
	return labels, nil
}

// evaluate runs `overload eval prs`: every workflow reviews every pull
// request's current head as a dry run on the overload server, which must
// be running to work the queue.
func evaluate(args []string) error {
	options, err := parseEvalOptions(args)
	if err != nil {
		return err
	}
	labels, err := readLabels(options.labels)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	repos, err := store.ListRepositories(ctx)
	if err != nil {
		return err
	}
	var installation, repositoryID int64 = -1, 0
	for _, repo := range repos {
		if repo.FullName != options.repo {
			continue
		}
		if repositoryID != 0 {
			return fmt.Errorf("%s matches more than one repository overload knows", options.repo)
		}
		installation, repositoryID = repo.InstallationID, repo.ID
	}
	if installation < 0 {
		return fmt.Errorf("%s is not a repository overload knows; add it first", options.repo)
	}
	// Evaluation reads pull requests from GitHub. When overload knows the
	// repository's GitHub ID, every pull request read must belong to it.
	githubID, err := store.RepositoryGitHubID(ctx, repositoryID)
	if err != nil {
		return err
	}
	var reader queue.HeadReader
	if installation > 0 {
		client, err := queue.AppClient(ctx, store)
		if err != nil {
			return fmt.Errorf("GitHub App unavailable: %w", err)
		}
		reader = client
	} else {
		token := github.TokenFromEnvironment()
		if token == "" {
			if token, err = github.TokenFromGH(ctx); err != nil {
				return err
			}
		}
		client, err := github.NewTokenClient(token)
		if err != nil {
			return err
		}
		reader = client
	}
	jobs, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		return err
	}
	batch := fmt.Sprintf("eval-%s", time.Now().UTC().Format("20060102T150405Z"))
	fmt.Fprintf(os.Stderr, "Evaluating %s on %s; the overload server runs the reviews as dry runs (batch %s)\n", strings.Join(options.workflows, ", "), overload.Plural(len(options.prs), "pull request"), batch)
	results, err := queue.RunEval(ctx, store, jobs, reader, queue.EvalPlan{Repository: options.repo, RepositoryID: repositoryID, GitHubID: githubID, InstallationID: installation, PRs: options.prs, Workflows: options.workflows, Concurrency: options.concurrency, Batch: batch, Progress: func(line string) { fmt.Fprintln(os.Stderr, line) }})
	if err != nil {
		return err
	}
	report := eval.BuildReport(options.repo, options.prs, options.workflows, results, labels)
	if err := writeEvalReport(report, options.out, options.csv); err != nil {
		return err
	}
	return printEvalSummary(os.Stdout, report)
}

func writeEvalReport(report eval.Report, jsonPath, csvPath string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(jsonPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	// Open without truncating and compare with the JSON file now that both
	// exist: a symlink can make the two paths one file.
	file, err := os.OpenFile(csvPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	csvInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if jsonInfo, err := os.Stat(jsonPath); err == nil && os.SameFile(jsonInfo, csvInfo) {
		_ = file.Close()
		return fmt.Errorf("%s and %s are the same file; the JSON results are in %s", csvPath, jsonPath, jsonPath)
	}
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return err
	}
	if err := report.WriteCSV(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func printEvalSummary(out io.Writer, report eval.Report) error {
	for _, summary := range report.Summary {
		recall := "no labels"
		if summary.Labels > 0 {
			recall = fmt.Sprintf("%d of %d labeled bugs (%.0f%%)", summary.Caught, summary.Labels, summary.Recall*100)
		}
		if _, err := fmt.Fprintf(out, "%s: %d/%d completed, %d degraded; caught %s; %.1f findings per PR; cost p50 %s, p90 %s, total %s; time p50 %.0fs, p90 %.0fs\n", summary.Workflow, summary.Completed, summary.Runs, summary.Degraded, recall, summary.FindingsPerPR, overload.FormatUSD(summary.CostP50USD), overload.FormatUSD(summary.CostP90USD), overload.FormatUSD(summary.CostTotalUSD), summary.SecondsP50, summary.SecondsP90); err != nil {
			return err
		}
	}
	return nil
}
