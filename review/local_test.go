package review

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

type fakePRSource struct {
	archive     []byte
	patch       string
	sha         string
	title, body string
}

func (source fakePRSource) PullRequestWithDiff(_ context.Context, _ string, number int) (overload.PullRequest, string, error) {
	return overload.PullRequest{Number: number, State: "open", HeadSHA: source.sha, BaseSHA: strings.Repeat("a", 40), Title: source.title, Body: source.body}, source.patch, nil
}

func (source fakePRSource) DownloadHead(_ context.Context, _ string, sha string, writer io.Writer) error {
	if sha != source.sha {
		return errors.New("wrong SHA")
	}
	_, err := writer.Write(source.archive)
	return err
}

type fakeReviewer struct{ seen bool }

func (reviewer *fakeReviewer) Review(_ context.Context, _ overload.ReviewSpec, repo fs.FS) (overload.ReviewResult, error) {
	data, err := fs.ReadFile(repo, "file.go")
	if err != nil || string(data) != "dangerous()\n" {
		return overload.ReviewResult{}, errors.New("head file not extracted")
	}
	reviewer.seen = true
	return overload.ReviewResult{Summary: "Found a bug", Findings: []overload.Finding{{Path: "file.go", Line: 1, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Bad call", Body: "Fix call", Confidence: .9, Evidence: "dangerous()"}}}, nil
}

func TestLocalRunner(t *testing.T) {
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
	source := fakePRSource{archive: archive.Bytes(), sha: strings.Repeat("b", 40), patch: "diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old()\n+dangerous()\n", title: "Make it dangerous", body: "Why: see <b>notes</b>"}
	reviewer := &fakeReviewer{}
	spec, result, sha, err := (LocalRunner{Source: source, Reviewer: reviewer}).Review(context.Background(), "owner/repo", 42)
	if err != nil || !reviewer.seen || len(result.Findings) != 1 || sha != source.sha || spec.PRNumber != 42 {
		t.Fatalf("local review: spec=%+v result=%+v sha=%s err=%v", spec, result, sha, err)
	}
	// The host's title and description reach the review, sanitized.
	if spec.PullRequest == nil || spec.PullRequest.Title != "Make it dangerous" || strings.Contains(spec.PullRequest.Body, "<b>") || !strings.Contains(spec.PullRequest.Body, "notes") {
		t.Fatalf("pull request info = %+v", spec.PullRequest)
	}
}
