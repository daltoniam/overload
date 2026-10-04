package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/harness"
	"github.com/daltoniam/overload/review"
	gh "github.com/google/go-github/v75/github"
)

type previewSource struct{ archive []byte }

func (source previewSource) PullRequestWithDiff(_ context.Context, _ string, number int) (*gh.PullRequest, string, error) {
	pr := &gh.PullRequest{Number: gh.Ptr(number), State: gh.Ptr("open"), Head: &gh.PullRequestBranch{SHA: gh.Ptr(strings.Repeat("b", 40))}, Base: &gh.PullRequestBranch{SHA: gh.Ptr(strings.Repeat("a", 40))}}
	return pr, "diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old()\n+dangerous()\n", nil
}

func (source previewSource) DownloadHead(_ context.Context, _, _ string, writer io.Writer) error {
	_, err := writer.Write(source.archive)
	return err
}

func TestLocalPreviewTimeoutValidation(t *testing.T) {
	for _, timeout := range []string{"0", "6h"} {
		t.Run(timeout, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", "not-a-real-token")
			t.Setenv("DATABASE_URL", "")
			err := inlineReview([]string{"--repo", "owner/repo", "--pr", "1", "--model", "bonsai-2-27b", "--model-url", "http://127.0.0.1:8080/v1", "--timeout", timeout})
			if err == nil || !strings.Contains(err.Error(), "timeout") {
				t.Fatalf("timeout %q accepted: %v", timeout, err)
			}
		})
	}
}

func TestLocalPreviewSavedProfileRequiresDatabase(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "not-a-real-token")
	t.Setenv("DATABASE_URL", "")
	err := inlineReview([]string{"--repo", "owner/repo", "--pr", "1", "--profile", "bonsai"})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL required for saved models") {
		t.Fatalf("missing database accepted: %v", err)
	}
}

func TestLocalPreviewWithFakeModel(t *testing.T) {
	var archive bytes.Buffer
	zip := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zip)
	file := []byte("dangerous()\n")
	if err := writer.WriteHeader(&tar.Header{Name: "repo-prefix/file.go", Mode: 0600, Size: int64(len(file))}); err != nil {
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
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected model route: %s", r.URL.Path)
		}
		content := `{"summary":"One actionable bug","findings":[{"path":"file.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bad call","body":"Fix the call","confidence":0.9,"evidence":"dangerous()"}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"fake","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`, content)
	}))
	defer model.Close()
	workflow, err := harness.ProfileWorkflow(overload.ModelProfile{Provider: "openaicompat", Model: "fake", BaseURL: model.URL}, "context")
	if err != nil {
		t.Fatal(err)
	}
	spec, result, sha, err := (review.LocalRunner{Source: previewSource{archive.Bytes()}, Reviewer: harness.Reviewer{}, Workflow: workflow}).Review(context.Background(), "owner/repo", 42)
	if err != nil || len(result.Findings) != 1 || calls != 1 || sha != strings.Repeat("b", 40) || spec.PRNumber != 42 {
		t.Fatalf("spec=%+v result=%+v sha=%s calls=%d error=%v", spec, result, sha, calls, err)
	}
}
