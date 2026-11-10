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
	// Models lets agents whose models need an API key reach them through
	// overload's model proxy; without it such agents cannot run sandboxed.
	Models ModelGrants
	// Rereview, when set, makes this an incremental re-review.
	Rereview *Rereview
}

// ModelGrants issues short-lived model URLs for sandboxes, so API keys stay
// in overload.
type ModelGrants interface {
	Grant(model overload.ModelProfile, ttl time.Duration) (baseURL string, revoke func())
}

// grantModels points agents whose models need an API key at the model
// proxy. It copies the agents so the run's pinned workflow is unchanged.
func (runner SandboxRunner) grantModels(ttl time.Duration) (overload.ResolvedWorkflow, func(), error) {
	workflow := runner.Workflow
	workflow.Agents = append([]overload.ResolvedAgent(nil), runner.Workflow.Agents...)
	var revokes []func()
	revokeAll := func() {
		for _, revoke := range revokes {
			revoke()
		}
	}
	for index, agent := range workflow.Agents {
		if agent.Model.APIKeyEnv == "" {
			continue
		}
		if runner.Models == nil {
			revokeAll()
			return workflow, nil, fmt.Errorf("agent %s needs API key %s; credentials are not sent into sandboxes, so enable the model proxy (OVERLOAD_MODEL_PROXY_URL)", agent.Name, agent.Model.APIKeyEnv)
		}
		base, revoke := runner.Models.Grant(agent.Model, ttl)
		revokes = append(revokes, revoke)
		agent.Model.BaseURL, agent.Model.APIKeyEnv, agent.Model.Headers = base, "", nil
		workflow.Agents[index] = agent
	}
	return workflow, revokeAll, nil
}

// The archive is extracted in its own command so its time is measured
// apart from the review.
const (
	sandboxExtractCommand = "set -e; mkdir repo; tar -xzf head.tar.gz -C repo --strip-components=1 --no-same-owner --no-same-permissions; rm head.tar.gz"
	sandboxReviewCommand  = "overload-agent review --spec spec.json --repo repo --out result.json --timeout %s"
)

func (runner SandboxRunner) Review(ctx context.Context, repo string, prNumber int) (overload.ReviewSpec, overload.ReviewResult, string, error) {
	if runner.Source == nil || runner.Sandbox == nil {
		return overload.ReviewSpec{}, overload.ReviewResult{}, "", errors.New("PR source and sandbox required")
	}
	workflow, revoke, err := runner.grantModels(maxAgentTimeout)
	if err != nil {
		return overload.ReviewSpec{}, overload.ReviewResult{}, "", err
	}
	defer revoke()
	timer := newTransferTimer()
	spec, archive, sha, err := fetchPR(ctx, runner.Source, workflow, repo, prNumber, runner.Rereview)
	if err != nil {
		return spec, overload.ReviewResult{}, sha, err
	}
	if spec.Tier != nil && spec.Tier.Skip {
		return spec, skippedReview(*spec.Tier), sha, nil
	}
	timer.lap("fetch_seconds")
	timer.metrics["archive_bytes"] = int64(archive.Len())
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return spec, overload.ReviewResult{}, sha, err
	}

	box, err := runner.Sandbox.Start(ctx, overload.SandboxOptions{RunID: runner.RunID})
	if err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("start sandbox: %w", err)
	}
	timer.lap("sandbox_start_seconds")
	defer func() { _ = box.Close(context.WithoutCancel(ctx)) }()
	if err := box.Upload(ctx, "spec.json", bytes.NewReader(specJSON)); err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("upload spec: %w", err)
	}
	if err := box.Upload(ctx, "head.tar.gz", archive); err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("upload archive: %w", err)
	}
	timer.lap("upload_seconds")
	extract, err := box.Exec(ctx, sandboxExtractCommand)
	if err != nil {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("sandbox extract: %w", err)
	}
	if extract.ExitCode != 0 {
		return spec, overload.ReviewResult{}, sha, fmt.Errorf("sandbox extract exited %d: %s", extract.ExitCode, overload.TruncateUTF8(extract.Stderr+extract.Stdout, 500))
	}
	timer.lap("extract_seconds")
	timeout := maxAgentTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline)-30*time.Second)
	}
	if timeout < time.Second {
		return spec, overload.ReviewResult{}, sha, errors.New("not enough time left to run the review")
	}
	exec, err := box.Exec(ctx, fmt.Sprintf(sandboxReviewCommand, timeout.Round(time.Second)))
	timer.lap("review_seconds")
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
	result, err = CheckResult(result, spec.Diff, spec.Workflow.FindingLimit())
	result = recordTier(spec, result)
	if result.Metrics == nil {
		result.Metrics = map[string]any{}
	}
	result.Metrics["sandbox"] = box.Name()
	result.Metrics["transfer"] = timer.metrics
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

// transferTimer records how long each step of getting the repository to
// the reviewer took, to judge whether large repositories need a faster
// transfer.
type transferTimer struct {
	last    time.Time
	metrics map[string]any
}

func newTransferTimer() *transferTimer {
	return &transferTimer{last: time.Now(), metrics: map[string]any{}}
}

func (timer *transferTimer) lap(name string) {
	now := time.Now()
	timer.metrics[name] = now.Sub(timer.last).Round(time.Millisecond).Seconds()
	timer.last = now
}
