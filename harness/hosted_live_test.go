package harness

import (
	"context"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/daltoniam/overload"
)

func TestHostedGatewaySmoke(t *testing.T) {
	if os.Getenv("OVERLOAD_HOSTED_SMOKE") != "1" {
		t.Skip("set OVERLOAD_HOSTED_SMOKE=1 for a live hosted smoke test")
	}
	if os.Getenv("CF_AIG_TOKEN") == "" {
		t.Skip("CF_AIG_TOKEN unavailable")
	}
	baseURL := os.Getenv("OVERLOAD_HOSTED_BASE_URL")
	if baseURL == "" {
		t.Skip("OVERLOAD_HOSTED_BASE_URL unavailable")
	}
	entry := "Review the supplied changed line. Return only JSON with summary and findings; report no findings when uncertain."
	workflow := overload.ResolvedWorkflow{Name: "hosted-smoke", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "hosted", Model: overload.ModelProfile{Provider: "openaicompat", ConnectionKind: "hosted", BaseURL: baseURL, Model: "gpt-6-sol", APIKeyEnv: "CF_AIG_TOKEN"}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
	spec := overload.ReviewSpec{Diff: "diff --git a/example.go b/example.go\n+++ b/example.go\n@@ -1 +1 @@\n-old\n+new\n", Workflow: workflow}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := (Reviewer{}).Review(ctx, spec, fstest.MapFS{})
	if err != nil {
		t.Fatalf("hosted review failed: %v", err)
	}
	if result.Metrics["reviewed_files"] != 1 {
		t.Fatalf("unexpected hosted review metrics: %+v", result.Metrics)
	}
}
