package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/daltoniam/overload"
)

type concurrencyModel struct {
	server   *httptest.Server
	inFlight atomic.Int64
	peak     atomic.Int64
	mu       sync.Mutex
	started  []int
}

var filePattern = regexp.MustCompile(`\+\+\+ b/f(\d+)\.go`)

// newConcurrencyModel answers each file review with one finding. Earlier
// files answer more slowly so completion order is the reverse of file order.
// fail, when non-negative, makes that file return HTTP 400 (not retried).
func newConcurrencyModel(t *testing.T, files, fail int) *concurrencyModel {
	t.Helper()
	model := &concurrencyModel{}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		match := filePattern.FindStringSubmatch(fmt.Sprint(request.Messages))
		if match == nil {
			t.Errorf("request without a file")
			return
		}
		index, _ := strconv.Atoi(match[1])
		model.mu.Lock()
		model.started = append(model.started, index)
		model.mu.Unlock()
		current := model.inFlight.Add(1)
		defer model.inFlight.Add(-1)
		for {
			peak := model.peak.Load()
			if current <= peak || model.peak.CompareAndSwap(peak, current) {
				break
			}
		}
		if index == fail {
			time.Sleep(20 * time.Millisecond)
			http.Error(w, "boom", http.StatusBadRequest)
			return
		}
		select {
		case <-time.After(time.Duration(files-index) * 15 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		content := fmt.Sprintf(`{"summary":"file %d","findings":[{"path":"f%d.go","line":1,"side":"RIGHT","severity":"high","category":"bug","title":"Bug %d","body":"b","confidence":0.9,"evidence":"bug%d()"}]}`, index, index, index, index)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`, content)
	}))
	t.Cleanup(model.server.Close)
	return model
}

func multiFileDiff(files int) string {
	var diff strings.Builder
	for index := range files {
		fmt.Fprintf(&diff, "diff --git a/f%d.go b/f%d.go\n--- a/f%d.go\n+++ b/f%d.go\n@@ -1 +1 @@\n-old()\n+bug%d()\n", index, index, index, index, index)
	}
	return diff.String()
}

func workflowSpec(baseURL string, files, concurrency int) overload.ReviewSpec {
	entry := "Entry"
	workflow := overload.ResolvedWorkflow{Name: "parallel", Kind: "pr_review", Revision: 1, Agents: []overload.ResolvedAgent{{Name: "local", Model: overload.ModelProfile{Provider: "openaicompat", ConnectionKind: "hosted", BaseURL: baseURL, Model: "m", Concurrency: concurrency}, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}}}}
	return overload.ReviewSpec{Diff: multiFileDiff(files), Workflow: workflow}
}

func findingTitles(result overload.ReviewResult) []string {
	titles := make([]string, 0, len(result.Findings))
	for _, finding := range result.Findings {
		titles = append(titles, finding.Title)
	}
	return titles
}

func TestParallelWorkflowReviewIsBoundedAndOrdered(t *testing.T) {
	const files = 7
	for _, test := range []struct {
		name        string
		concurrency int
		wantPeak    int64
	}{
		{"default is sequential", 0, 1},
		{"three at once", 3, 3},
		{"more workers than files", 20, files},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newConcurrencyModel(t, files, -1)
			result, err := (Reviewer{}).Review(context.Background(), workflowSpec(model.server.URL, files, test.concurrency), fstest.MapFS{})
			if err != nil {
				t.Fatal(err)
			}
			if got := model.peak.Load(); got != test.wantPeak {
				t.Fatalf("peak in-flight requests %d, want %d", got, test.wantPeak)
			}
			want := []string{"Bug 0", "Bug 1", "Bug 2", "Bug 3", "Bug 4", "Bug 5", "Bug 6"}
			if got := findingTitles(result); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("findings not in file order: %v", got)
			}
			if result.Metrics["reviewed_files"] != files || result.Metrics["agent_local_reviewed_files"] != files || result.Metrics["output_tokens"] != int64(5*files) || result.Metrics["input_tokens"] != int64(10*files) || result.Metrics["cached_input_tokens"] != int64(4*files) || result.Metrics["raw_candidates"] != files {
				t.Fatalf("metrics %v", result.Metrics)
			}
			if !strings.HasPrefix(result.Summary, "file 0\nfile 1") {
				t.Fatalf("summary not in file order: %q", result.Summary)
			}
		})
	}
}

func TestParallelWorkflowStopsOnFirstFailure(t *testing.T) {
	const files = 12
	model := newConcurrencyModel(t, files, 1)
	_, err := (Reviewer{}).Review(context.Background(), workflowSpec(model.server.URL, files, 3), fstest.MapFS{})
	if err == nil || !strings.Contains(err.Error(), "f1.go") {
		t.Fatalf("error does not name the failing file: %v", err)
	}
	model.mu.Lock()
	started := len(model.started)
	model.mu.Unlock()
	if started >= files {
		t.Fatalf("all %d files started after an early failure", started)
	}
}

func TestParallelSavedModelWithPasses(t *testing.T) {
	const files = 6
	model := newConcurrencyModel(t, files, -1)
	spec := profileSpec(t, multiFileDiff(files), overload.ModelProfile{ConnectionKind: "hosted", APIKeyEnv: "TEST_KEY", BaseURL: model.server.URL, Model: "m", Concurrency: 4}, "context", []overload.ReviewAgent{{Name: "security", Instructions: "Look for bugs."}, {Name: "style", Instructions: "Look for bugs too."}})
	result, err := (Reviewer{}).Review(context.Background(), spec, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	if got := model.peak.Load(); got != 4 {
		t.Fatalf("peak in-flight requests %d, want 4", got)
	}
	if got := findingTitles(result); len(got) != files || got[0] != "Bug 0" || got[files-1] != "Bug 5" {
		t.Fatalf("findings %v", got)
	}
	if result.Metrics["raw_candidates"] != 2*files || result.Metrics["agent_security_concurrency"] != 4 || result.Metrics["agent_style_concurrency"] != 4 {
		t.Fatalf("metrics %v", result.Metrics)
	}
}

func TestForEachFilePrefersRealErrorOverCancellation(t *testing.T) {
	boom := errors.New("boom")
	err := forEachFile(context.Background(), 8, 4, func(ctx context.Context, index int) error {
		if index == 2 {
			return boom
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	if err := forEachFile(ctx, 5, 2, func(context.Context, int) error { calls++; return nil }); !errors.Is(err, context.Canceled) || calls > 2 {
		t.Fatalf("cancelled context: err=%v calls=%d", err, calls)
	}
}
