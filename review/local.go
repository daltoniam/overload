package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/daltoniam/overload"
	gh "github.com/google/go-github/v75/github"
)

type PullRequestSource interface {
	PullRequestWithDiff(context.Context, string, int) (*gh.PullRequest, string, error)
	DownloadHead(context.Context, string, string, io.Writer) error
}

// Runner reviews one pull request with a pinned workflow and returns the
// spec it reviewed, the result and the head SHA.
type Runner interface {
	Review(ctx context.Context, repo string, prNumber int) (overload.ReviewSpec, overload.ReviewResult, string, error)
}

// fetchPR loads an open pull request, its diff and head archive and builds
// the spec both runners review.
func fetchPR(ctx context.Context, source PullRequestSource, workflow overload.ResolvedWorkflow, repo string, prNumber int) (overload.ReviewSpec, *bytes.Buffer, string, error) {
	pr, patch, err := source.PullRequestWithDiff(ctx, repo, prNumber)
	if err != nil {
		return overload.ReviewSpec{}, nil, "", fmt.Errorf("fetch PR: %w", err)
	}
	if err := checkPR(pr, prNumber, patch); err != nil {
		return overload.ReviewSpec{}, nil, pr.GetHead().GetSHA(), err
	}
	sha := pr.GetHead().GetSHA()
	spec := overload.ReviewSpec{Repository: overload.Repository{FullName: repo}, PRNumber: prNumber, Diff: patch, Workflow: workflow}
	archive := &bytes.Buffer{}
	if err := source.DownloadHead(ctx, repo, sha, archive); err != nil {
		return spec, nil, sha, fmt.Errorf("download head: %w", err)
	}
	return spec, archive, sha, nil
}

func checkPR(pr *gh.PullRequest, prNumber int, patch string) error {
	if pr == nil || pr.GetHead().GetSHA() == "" || pr.GetBase().GetSHA() == "" {
		return errors.New("PR missing head or base SHA")
	}
	if pr.GetNumber() != prNumber || pr.GetState() != "open" {
		return errors.New("PR is not open or number does not match")
	}
	if len(patch) == 0 || len(patch) > 2<<20 {
		return errors.New("PR diff is empty or exceeds 2 MiB")
	}
	return nil
}

// LocalRunner extracts the head archive into a temporary directory and
// reviews it in this process.
type LocalRunner struct {
	Source   PullRequestSource
	Reviewer overload.Reviewer
	Workflow overload.ResolvedWorkflow
}

func (runner LocalRunner) Review(ctx context.Context, repo string, prNumber int) (overload.ReviewSpec, overload.ReviewResult, string, error) {
	if runner.Source == nil || runner.Reviewer == nil {
		return overload.ReviewSpec{}, overload.ReviewResult{}, "", errors.New("PR source and reviewer required")
	}
	spec, archive, sha, err := fetchPR(ctx, runner.Source, runner.Workflow, repo, prNumber)
	if err != nil {
		return spec, overload.ReviewResult{}, sha, err
	}
	workspace, err := os.MkdirTemp("", "overload-pr-")
	if err != nil {
		return spec, overload.ReviewResult{}, sha, err
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	if err := ExtractArchive(ctx, bytes.NewReader(archive.Bytes()), workspace); err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("extract head: %w", err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return spec, overload.ReviewResult{}, sha, err
	}
	defer func() { _ = root.Close() }()
	result, err := runner.Reviewer.Review(ctx, spec, root.FS())
	if err != nil {
		return spec, result, sha, err
	}
	result.Findings, err = Validate(result.Findings, spec.Diff, 10)
	return spec, result, sha, err
}
