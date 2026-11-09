package review

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

type countingSource struct {
	fakePRSource
	downloads int
}

func (source *countingSource) DownloadHead(ctx context.Context, repo, sha string, writer io.Writer) error {
	source.downloads++
	return source.fakePRSource.DownloadHead(ctx, repo, sha, writer)
}

type agentsReviewer struct{ agents []string }

func (reviewer *agentsReviewer) Review(_ context.Context, spec overload.ReviewSpec, _ fs.FS) (overload.ReviewResult, error) {
	for _, agent := range spec.Workflow.Agents {
		reviewer.agents = append(reviewer.agents, agent.Name)
	}
	return overload.ReviewResult{Findings: []overload.Finding{}, Metrics: map[string]any{}}, nil
}

func TestTiersSkipOrNarrowBeforeAnyModel(t *testing.T) {
	entry := overload.PromptTemplate{Kind: "entry", Body: "Review.", SHA256: overload.PromptDigest("Review.")}
	model := overload.ModelProfile{Provider: "openaicompat", BaseURL: "http://127.0.0.1:1/v1", Model: "m"}
	workflow := overload.ResolvedWorkflow{Version: overload.SnapshotVersion, Name: "tiered", Kind: "pr_review", Agents: []overload.ResolvedAgent{{Name: "lead", Model: model, EntryPrompt: entry}, {Name: "security", Model: model, EntryPrompt: entry}}, ReviewPolicy: overload.ReviewPolicy{Tiers: &overload.Tiers{
		Trivial: &overload.Tier{MaxLines: 2, MaxFiles: 1, Action: overload.TierSkip},
		Lite:    &overload.Tier{MaxLines: 50, MaxFiles: 5, Agents: []string{"main"}},
	}}}
	small := "diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old()\n+dangerous()\n"
	medium := small + "diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1,2 @@\n x\n+y\n+z\n"
	source := &countingSource{fakePRSource: fakePRSource{archive: testArchive(t), patch: small, sha: strings.Repeat("b", 40)}}
	reviewer := &agentsReviewer{}
	spec, result, _, err := LocalRunner{Source: source, Reviewer: reviewer, Workflow: workflow}.Review(context.Background(), "acme/api", 3)
	if err != nil || source.downloads != 0 || len(reviewer.agents) != 0 {
		t.Fatalf("trivial change reviewed: err=%v downloads=%d agents=%v", err, source.downloads, reviewer.agents)
	}
	if result.Metrics["tier"] != overload.TierTrivial || result.Metrics["tier_skipped"] != true || spec.Tier == nil || !strings.Contains(result.Summary, "Trivial change (2 changed lines in 1 file)") {
		t.Fatalf("result = %+v", result)
	}
	source.patch = medium
	_, result, _, err = LocalRunner{Source: source, Reviewer: reviewer, Workflow: workflow}.Review(context.Background(), "acme/api", 3)
	if err != nil || strings.Join(reviewer.agents, ",") != "lead" || result.Metrics["tier"] != overload.TierLite {
		t.Fatalf("lite review: err=%v agents=%v metrics=%+v", err, reviewer.agents, result.Metrics)
	}
	if source.downloads != 1 {
		t.Fatalf("downloads = %d", source.downloads)
	}
}

func testArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	zip := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zip)
	file := []byte("dangerous()\n")
	if err := writer.WriteHeader(&tar.Header{Name: "repo-sha/file.go", Mode: 0600, Size: int64(len(file))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(file); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
