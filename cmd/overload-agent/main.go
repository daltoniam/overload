package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/harness"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "review" {
		return errors.New("usage: overload-agent review --spec path --repo path --out path")
	}
	flags := flag.NewFlagSet("review", flag.ContinueOnError)
	specPath := flags.String("spec", "", "review spec")
	repoPath := flags.String("repo", "", "repository")
	outPath := flags.String("out", "", "result output")
	timeout := flags.Duration("timeout", 15*time.Minute, "maximum review duration (up to 5h)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *specPath == "" || *repoPath == "" || *outPath == "" {
		return errors.New("spec, repo and out required")
	}
	if !validReviewTimeout(*timeout) {
		return errors.New("review timeout must be between 1 second and 5 hours")
	}
	data, err := os.ReadFile(*specPath)
	if err != nil {
		return err
	}
	var spec overload.ReviewSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	if headers := os.Getenv("OVERLOAD_MODEL_HEADERS_JSON"); headers != "" {
		var parsed map[string]string
		if err := json.Unmarshal([]byte(headers), &parsed); err != nil {
			return errors.New("invalid OVERLOAD_MODEL_HEADERS_JSON")
		}
		for index := range spec.Workflow.Agents {
			spec.Workflow.Agents[index].Model.Headers = parsed
		}
	}
	root, err := os.OpenRoot(*repoPath)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := (harness.Reviewer{}).Review(ctx, spec, root.FS())
	if err != nil {
		result.Error = err.Error()
	}
	output, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(*outPath, output, 0600)
}

func validReviewTimeout(timeout time.Duration) bool {
	return timeout >= time.Second && timeout <= 5*time.Hour
}
