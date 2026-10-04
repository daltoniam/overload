package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/daltoniam/overload"
)

// maxAgentTimeout mirrors the overload-agent upper bound.
const maxAgentTimeout = 5 * time.Hour

// SandboxRunner fetches PR data on the control plane but never extracts or
// reads the untrusted archive there. Extraction and model calls happen inside
// a sandbox; only the JSON result comes back, and findings are re-validated
// against the diff before use.
type SandboxRunner struct {
	Source   PullRequestSource
	Sandbox  overload.SandboxRunner
	Workflow overload.ResolvedWorkflow
	RunID    int64
}

const sandboxReviewCommand = "set -e; mkdir repo; tar -xzf head.tar.gz -C repo --strip-components=1 --no-same-owner --no-same-permissions; rm head.tar.gz; overload-agent review --spec spec.json --repo repo --out result.json --timeout %s"

func (runner SandboxRunner) Review(ctx context.Context, repo string, prNumber int) (overload.ReviewSpec, overload.ReviewResult, string, error) {
	if runner.Source == nil || runner.Sandbox == nil {
		return overload.ReviewSpec{}, overload.ReviewResult{}, "", errors.New("PR source and sandbox required")
	}
	for _, agent := range runner.Workflow.Agents {
		if agent.Model.APIKeyEnv != "" {
			return overload.ReviewSpec{}, overload.ReviewResult{}, "", fmt.Errorf("agent %s needs API key %s; credentials are not sent into sandboxes", agent.Name, agent.Model.APIKeyEnv)
		}
	}
	spec, archive, sha, err := fetchPR(ctx, runner.Source, runner.Workflow, repo, prNumber)
	if err != nil {
		return spec, overload.ReviewResult{}, sha, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return spec, overload.ReviewResult{}, sha, err
	}

	box, err := runner.Sandbox.Start(ctx, overload.SandboxOptions{RunID: runner.RunID})
	if err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("start sandbox: %w", err)
	}
	defer func() { _ = box.Close(context.WithoutCancel(ctx)) }()
	if err := box.Upload(ctx, "spec.json", bytes.NewReader(specJSON)); err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("upload spec: %w", err)
	}
	if err := box.Upload(ctx, "head.tar.gz", archive); err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("upload archive: %w", err)
	}
	timeout := maxAgentTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline)-30*time.Second)
	}
	if timeout < time.Second {
		return spec, overload.ReviewResult{}, sha, errors.New("not enough time left to run the review")
	}
	exec, err := box.Exec(ctx, fmt.Sprintf(sandboxReviewCommand, timeout.Round(time.Second)))
	if err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("sandbox exec: %w", err)
	}
	if exec.ExitCode != 0 {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("sandbox review exited %d: %s", exec.ExitCode, overload.TruncateUTF8(exec.Stderr+exec.Stdout, 500))
	}
	output := &limitedBuffer{limit: 8 << 20}
	if _, err := box.Download(ctx, "result.json", output); err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("download result: %w", err)
	}
	var result overload.ReviewResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		return spec, result, sha, fmt.Errorf("invalid sandbox result: %w", err)
	}
	if result.Error != "" {
		return spec, result, sha, nil
	}
	result.Findings, err = Validate(result.Findings, spec.Diff, 10)
	if result.Metrics == nil {
		result.Metrics = map[string]any{}
	}
	result.Metrics["sandbox"] = box.Name()
	return spec, result, sha, err
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	if buffer.Len()+len(data) > buffer.limit {
		return 0, errors.New("sandbox result too large")
	}
	return buffer.Buffer.Write(data)
}
