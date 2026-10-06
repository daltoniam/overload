package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/daltoniam/overload"
)

func TestContextBundleBounded(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	content := strings.Repeat("repository context\n", 4000)
	bundle, err := firstBatch(patch, fstest.MapFS{"a.go": &fstest.MapFile{Data: []byte(content)}})
	if err != nil || len(bundle) > 32000 || !strings.Contains(bundle, "+new") {
		t.Fatalf("bundle length=%d error=%v", len(bundle), err)
	}
	largeBundle, err := firstBatch(strings.Repeat(patch, 600), fstest.MapFS{})
	if err != nil || len(largeBundle) > 32000 {
		t.Fatalf("large diff not bounded: length=%d error=%v", len(largeBundle), err)
	}
	if _, err := firstBatch(strings.Repeat(patch, 40000), fstest.MapFS{}); err == nil {
		t.Fatal("diff beyond fetch limit accepted")
	}
}

func TestReviewerCoversAllFiles(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		path := "a.go"
		if strings.Contains(string(body), "b.go") {
			path = "b.go"
		}
		content := fmt.Sprintf(`{"summary":"Bug in %s","findings":[{"path":%q,"line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bug","body":"Fix it","confidence":0.9,"evidence":"bad()"}]}`, path, path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, content)
	}))
	defer server.Close()
	patch := "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+bad()\ndiff --git a/b.go b/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-old\n+bad()\n"
	result, err := (Reviewer{}).Review(context.Background(), profileSpec(t, patch, overload.ModelProfile{BaseURL: server.URL, Model: "test"}, "context"), fstest.MapFS{})
	if err != nil || calls != 2 || len(result.Findings) != 2 || result.Metrics["reviewed_files"] != 2 || result.Metrics["total_files"] != 2 {
		t.Fatalf("result=%+v calls=%d error=%v", result, calls, err)
	}
}

func TestReviewerRunsMultipleAgentsAndDeduplicates(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(body), []string{"Check security", "Check correctness"}[calls-1]) {
			t.Errorf("wrong agent focus: %v", err)
		}
		content := `{"summary":"Found a bug","findings":[{"path":"a.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bug","body":"Fix it","confidence":0.9,"evidence":"bad()"}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, content)
	}))
	defer server.Close()
	spec := focusedSpec(t, "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+bad()\n", overload.ModelProfile{BaseURL: server.URL, Model: "test"}, "security", "Check security", "correctness", "Check correctness")
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
	if err != nil || calls != 2 || len(result.Findings) != 1 || result.Metrics["agents"] != 2 || result.Metrics["agent_security_reviewed_files"] != 1 || result.Metrics["agent_correctness_reviewed_files"] != 1 {
		t.Fatalf("calls=%d result=%+v error=%v", calls, result, err)
	}
}

func TestReviewerWithVersionedModelEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected model endpoint: %s", r.URL.Path)
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "bonsai-2-27b" {
			t.Errorf("unexpected model request: %+v, %v", request, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"bonsai-2-27b","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, `{"summary":"","findings":[]}`)
	}))
	defer server.Close()
	spec := profileSpec(t, "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n", overload.ModelProfile{BaseURL: server.URL + "/v1", Model: "bonsai-2-27b"}, "context")
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
	if err != nil || result.Metrics["reviewed_files"] != 1 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestReviewerUsesNamedProjectProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(body), "compaction allow-lists") {
			t.Errorf("project prompt absent: error=%v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, `{"summary":"","findings":[]}`)
	}))
	defer server.Close()
	patch := "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	result, err := (Reviewer{}).Review(context.Background(), profileSpec(t, patch, overload.ModelProfile{BaseURL: server.URL, Model: "test"}, "switchboard-go"), fstest.MapFS{})
	if err != nil || result.Metrics["workflow"] != "switchboard-go-v1" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestReviewerDoesNotClaimPartialCoverage(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls >= 2 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, `{"summary":"Nothing found","findings":[]}`)
	}))
	defer server.Close()
	patch := "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\ndiff --git a/b.go b/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-old\n+new\n"
	result, err := (Reviewer{}).Review(context.Background(), profileSpec(t, patch, overload.ModelProfile{BaseURL: server.URL, Model: "test"}, "context"), fstest.MapFS{})
	if err == nil || result.Metrics["agent_reviewer_reviewed_files"] != 1 || result.Metrics["total_files"] != 2 {
		t.Fatalf("partial result=%+v error=%v", result, err)
	}
}

func TestReviewerClearsUnvalidatedSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, `{"summary":"A critical bug exists","findings":[{"path":"missing.go","line":99,"side":"RIGHT","severity":"critical","category":"bug","title":"Missing","body":"Bad","confidence":1,"evidence":"bad()"}]}`)
	}))
	defer server.Close()
	patch := "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	result, err := (Reviewer{}).Review(context.Background(), profileSpec(t, patch, overload.ModelProfile{BaseURL: server.URL, Model: "test"}, "context"), fstest.MapFS{})
	if err != nil || len(result.Findings) != 0 || result.Summary != "No actionable findings in reviewed files." || result.Metrics["raw_candidates"] != 1 || result.Metrics["validated_candidates"] != 0 {
		t.Fatalf("unexpected result: %+v error: %v", result, err)
	}
}

func TestReviewerModelHeaders(t *testing.T) {
	const secret = "private-example-key"
	t.Setenv("OVERLOAD_TEST_MODEL_KEY", secret)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("Cf-Aig-Metadata") != `{"source":"paired-review"}` {
			t.Errorf("missing expected gateway headers")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, `{"summary":"","findings":[]}`)
	}))
	defer server.Close()
	profile := overload.ModelProfile{BaseURL: server.URL, Model: "test", APIKeyEnv: "OVERLOAD_TEST_MODEL_KEY", Headers: map[string]string{"cf-aig-metadata": `{"source":"paired-review"}`}}
	spec := profileSpec(t, "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n", profile, "context")
	encoded, err := json.Marshal(spec)
	if err != nil || strings.Contains(string(encoded), "cf-aig-metadata") || strings.Contains(string(encoded), secret) {
		t.Fatalf("credentials serialized into review spec: %v", err)
	}
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
	if err != nil || calls != 1 || result.Metrics["reviewed_files"] != 1 {
		t.Fatalf("calls=%d result=%+v error=%v", calls, result, err)
	}
	for _, headers := range []map[string]string{
		{"authorization": "Bearer forbidden"},
		{"cf-aig-metadata": "invalid\nvalue"},
		{"cf-aig-metadata": strings.Repeat("x", 4097)},
		{"Cf-Aig-Metadata": "wrong case"},
	} {
		spec.Workflow.Agents[0].Model.Headers = headers
		if _, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{}); err == nil || calls != 1 || strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("invalid headers accepted or leaked: calls=%d error=%v", calls, err)
		}
	}
}

func TestReviewerRepairsInvalidJSON(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if calls == 1 && !strings.Contains(string(body), "dangerous()") {
			t.Error("diff omitted from initial request")
		}
		content := `{"summary":"Found a bug","findings":[{"path":"a.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bug","body":"Fix it","confidence":0.9,"evidence":"dangerous()"}]}`
		if calls == 1 {
			content = "invalid"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chat-1","object":"chat.completion","created":123,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`, content)
	}))
	defer server.Close()
	patch := "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+dangerous()\n"
	spec := profileSpec(t, patch, overload.ModelProfile{BaseURL: server.URL, Model: "test"}, "context")
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{"a.go": &fstest.MapFile{Data: []byte("dangerous()\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(result.Findings) != 1 {
		t.Fatalf("calls=%d, result=%+v", calls, result)
	}
}

func firstBatch(patch string, repo fs.FS) (string, error) {
	parsed, err := parseReviewDiff(patch)
	if err != nil {
		return "", err
	}
	return makeReviewBatches(parsed, repo, parsed.paths()[:1])[0], nil
}
