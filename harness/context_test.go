package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"charm.land/fantasy"
	"github.com/daltoniam/overload"
)

// renameFixture renames fetch_user to load_user in svc/users.py; the head
// still calls fetch_user in api/handlers.py, which the diff does not touch.
const renameDiff = "diff --git a/svc/users.py b/svc/users.py\n--- a/svc/users.py\n+++ b/svc/users.py\n@@ -1,6 +1,6 @@\n import db\n \n \n-def fetch_user(user_id):\n+def load_user(user_id):\n     return db.get(user_id)\ndiff --git a/svc/util.py b/svc/util.py\n--- a/svc/util.py\n+++ b/svc/util.py\n@@ -1,2 +1,3 @@\n def helper():\n     return 1\n+# note\n"

func renameRepo() fstest.MapFS {
	return fstest.MapFS{
		"svc/users.py":      {Data: []byte("import db\n\n\ndef load_user(user_id):\n    return db.get(user_id)\n")},
		"svc/util.py":       {Data: []byte("def helper():\n    return 1\n# note\n")},
		"api/handlers.py":   {Data: []byte("from svc import users\n\n\ndef show(request):\n    user_id = request.args['id']\n    return users.fetch_user(user_id)\n")},
		"node_modules/x.js": {Data: []byte("fetch_user()\n")},
	}
}

func TestContractsIndexListsCallersOutsideTheDiff(t *testing.T) {
	parsed, err := parseReviewDiff(renameDiff)
	if err != nil {
		t.Fatal(err)
	}
	index := buildContractsIndex(context.Background(), parsed, renameRepo(), 8)
	if len(index.Changes) != 1 {
		t.Fatalf("changes = %+v", index.Changes)
	}
	change := index.Changes[0]
	if change.Symbol != "fetch_user" || change.Kind != "removed" || change.Path != "svc/users.py" || len(change.Uses) != 1 || change.Uses[0].Path != "api/handlers.py" || change.Uses[0].Line != 6 || change.Uses[0].InDiff {
		t.Fatalf("change = %+v", change)
	}
	rendered := index.render(10000)
	if !strings.Contains(rendered, "api/handlers.py:6: return users.fetch_user(user_id)") || !strings.Contains(rendered, "fetch_user removed in svc/users.py") {
		t.Fatalf("rendered:\n%s", rendered)
	}
	if links := index.linked(); len(links["svc/users.py"]) != 1 || links["svc/users.py"][0] != "api/handlers.py" {
		t.Fatalf("links = %+v", links)
	}
}

func TestContractsIndexSignatureChangesAndLanguages(t *testing.T) {
	for _, test := range []struct {
		name, diff, file, symbol, kind string
	}{
		{"go signature", "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-func Charge(amount int) error {\n+func Charge(amount int, currency string) error {\n", "a.go", "Charge", "changed"},
		{"go method moved unchanged", "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,2 @@\n-func (s *Store) Load() error {\n+// moved\n diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1,2 @@\n x\n+func (s *Store) Load() error {\n", "", "", ""},
		{"typescript export", "diff --git a/a.ts b/a.ts\n--- a/a.ts\n+++ b/a.ts\n@@ -1 +1 @@\n-export function formatPrice(cents: number): string {\n+export function formatAmount(cents: number): string {\n", "a.ts", "formatPrice", "removed"},
		{"graphql field", "diff --git a/schema.graphql b/schema.graphql\n--- a/schema.graphql\n+++ b/schema.graphql\n@@ -1,3 +1,3 @@\n type Candidate {\n-  fullName: String!\n+  displayName: String!\n }\n", "schema.graphql", "fullName", "removed"},
		{"python class", "diff --git a/m.py b/m.py\n--- a/m.py\n+++ b/m.py\n@@ -1 +1 @@\n-class Interview(Base):\n+class InterviewSession(Base):\n", "m.py", "Interview", "removed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseReviewDiff(test.diff)
			if err != nil {
				t.Fatal(err)
			}
			index := buildContractsIndex(context.Background(), parsed, fstest.MapFS{}, 8)
			if test.symbol == "" {
				if len(index.Changes) != 0 {
					t.Fatalf("unchanged move listed: %+v", index.Changes)
				}
				return
			}
			if len(index.Changes) != 1 || index.Changes[0].Symbol != test.symbol || index.Changes[0].Kind != test.kind || index.Changes[0].Path != test.file {
				t.Fatalf("changes = %+v", index.Changes)
			}
		})
	}
}

type capturedRequests struct {
	mu     sync.Mutex
	bodies []string
}

func (captured *capturedRequests) server(t *testing.T, answer func(body string) string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		captured.mu.Lock()
		captured.bodies = append(captured.bodies, string(data))
		captured.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":2000,"completion_tokens":100,"total_tokens":2100,"prompt_tokens_details":{"cached_tokens":1500}}}`, answer(string(data)))
	}))
	t.Cleanup(server.Close)
	return server
}

func contextSpec(baseURL string) overload.ReviewSpec {
	spec := workflowSpec(baseURL, 0, 1)
	spec.Diff = renameDiff
	spec.Workflow.Version = overload.SnapshotVersion
	spec.Workflow.Context = &overload.ContextSettings{}
	spec.PullRequest = &overload.PullRequestInfo{Title: "Rename fetch_user", Body: "Clearer name."}
	return spec
}

func TestFileReviewsShareACacheablePrefix(t *testing.T) {
	var captured capturedRequests
	server := captured.server(t, func(string) string { return `{"summary":"ok","findings":[]}` })
	result, err := (Reviewer{}).Review(context.Background(), contextSpec(server.URL), renameRepo())
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.bodies) != 2 {
		t.Fatalf("calls = %d", len(captured.bodies))
	}
	var prefixes []string
	for _, body := range captured.bodies {
		cut := strings.Index(body, "Review this changed file")
		if cut < 0 || !strings.Contains(body[:cut], "api/handlers.py:6") || !strings.Contains(body[:cut], "Title: Rename fetch_user") {
			t.Fatalf("bundle not before the file section:\n%s", body)
		}
		prefixes = append(prefixes, body[:cut])
	}
	if prefixes[0] != prefixes[1] {
		t.Fatalf("prompt prefixes differ:\n%s\n---\n%s", prefixes[0], prefixes[1])
	}
	if result.Metrics["contracts_symbols"] != 1 || result.Metrics["cached_input_tokens"] != int64(3000) {
		t.Fatalf("metrics = %+v", result.Metrics)
	}
}

func TestAnthropicMarksTheBundleCacheable(t *testing.T) {
	plan := reasoningPlan{provider: overload.ProviderAnthropic}
	call := reviewCall(plan, "bundle", "file")
	data, err := json.Marshal(call.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "cache_control") || len(call.Messages[0].Content) != 2 {
		t.Fatalf("messages = %s", data)
	}
	if openai := reviewCall(reasoningPlan{}, "bundle", "file"); strings.Contains(fmt.Sprint(openai.Messages[0].Content[0]), "cache_control") {
		t.Fatal("OpenAI call carries Anthropic cache control")
	}
	if plain := reviewCall(plan, "", "file"); plain.Prompt != "file" || len(plain.Messages) != 0 {
		t.Fatalf("call without a bundle = %+v", plain)
	}
}

func TestWholePRAgentFindsCrossFileBug(t *testing.T) {
	recorded, err := os.ReadFile("testdata/wholepr/response.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured capturedRequests
	server := captured.server(t, func(body string) string {
		if strings.Contains(body, "Review these changed files together") {
			return string(recorded)
		}
		return `{"summary":"ok","findings":[]}`
	})
	spec := contextSpec(server.URL)
	spec.Workflow.Context = nil
	entry := "Find cross-file bugs."
	spec.Workflow.Agents = append(spec.Workflow.Agents, overload.ResolvedAgent{Name: "correctness", Model: spec.Workflow.Agents[0].Model, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}, Scope: overload.Scope{Mode: overload.ScopeWholePR}})
	result, err := (Reviewer{}).Review(context.Background(), spec, renameRepo())
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.bodies) != 3 {
		t.Fatalf("calls = %d (want two file reviews and one whole-PR call)", len(captured.bodies))
	}
	if len(result.Findings) != 1 || result.Findings[0].Path != "svc/users.py" || result.Findings[0].Line != 4 || result.Findings[0].Agents[0] != "correctness" {
		t.Fatalf("findings = %+v", result.Findings)
	}
	routing := result.Metrics["routing"].(overload.Routing)
	whole := routing.Agents[1]
	if !whole.WholePR || whole.Reviewed != 2 || whole.Findings != 1 || len(routing.Degraded) != 0 {
		t.Fatalf("routing = %+v", routing)
	}
	if routing.Agents[0].Reviewed != 2 {
		t.Fatalf("main agent lost files to the whole-PR agent: %+v", routing.Agents[0])
	}
}

func TestGroupWholePR(t *testing.T) {
	section := func(name string, size int) string {
		return fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1 +1 @@\n-x\n+%s\n", name, name, name, name, strings.Repeat("y", size))
	}
	diff := section("api/a.py", 3000) + section("api/b.py", 3000) + section("web/c.ts", 3000) + section("db/d.py", 3000) + section("huge/e.py", 20000)
	parsed, err := parseReviewDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	files := parsed.paths()
	groups, left := groupWholePR(parsed, files[:4], nil, 100000)
	if len(groups) != 1 || len(groups[0]) != 4 || len(left) != 0 {
		t.Fatalf("under budget: %v %v", groups, left)
	}
	groups, left = groupWholePR(parsed, files, map[string][]string{"web/c.ts": {"db/d.py"}}, 7000+len(wholePRHeader))
	want := "[[api/a.py api/b.py] [web/c.ts db/d.py]]"
	if fmt.Sprint(groups) != want || fmt.Sprint(left) != "[huge/e.py]" {
		t.Fatalf("groups = %v left = %v, want %s", groups, left, want)
	}
}

func TestRepoToolsStayInsideTheRepository(t *testing.T) {
	tools := newRepoTools(renameRepo(), 4)
	byName := map[string]func(string) (string, bool){}
	for _, tool := range tools.agentTools() {
		byName[tool.Info().Name] = func(input string) (string, bool) {
			response, err := tool.Run(context.Background(), fantasyCall(tool.Info().Name, input))
			if err != nil {
				t.Fatal(err)
			}
			return response.Content, response.IsError
		}
	}
	if text, isError := byName["read_file"](`{"path":"api/handlers.py","start":6,"end":6}`); isError || text != "6:     return users.fetch_user(user_id)\n" {
		t.Fatalf("read_file = %q %v", text, isError)
	}
	if _, isError := byName["read_file"](`{"path":"../etc/passwd"}`); !isError {
		t.Fatal("read outside the repository")
	}
	if text, _ := byName["search"](`{"pattern":"fetch_user","glob":"**/*.py"}`); text != "api/handlers.py:6: return users.fetch_user(user_id)\n" {
		t.Fatalf("search = %q", text)
	}
	if text, _ := byName["list_dir"](`{"path":"svc"}`); text != "svc/users.py\nsvc/util.py" {
		t.Fatalf("list_dir = %q", text)
	}
	if text, isError := byName["list_dir"](`{}`); !isError || !strings.Contains(text, "limit reached") {
		t.Fatalf("fifth call = %q %v", text, isError)
	}
}

func fantasyCall(name, input string) fantasy.ToolCall {
	return fantasy.ToolCall{ID: "call-1", Name: name, Input: input}
}

func TestWholePRAgentCanReadTheRepository(t *testing.T) {
	recorded, err := os.ReadFile("testdata/wholepr/response.json")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var toolResult string
	wholeCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body := string(data)
		w.Header().Set("Content-Type", "application/json")
		usage := `"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050}`
		if !strings.Contains(body, "Review these changed files together") {
			_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"summary\":\"ok\",\"findings\":[]}"}}],%s}`, usage)
			return
		}
		mu.Lock()
		wholeCalls++
		mu.Unlock()
		if !strings.Contains(body, `"role":"tool"`) {
			_, _ = fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"api/handlers.py\"}"}}]}}],%s}`, usage)
			return
		}
		mu.Lock()
		toolResult = body
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"id":"2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%q}}],%s}`, string(recorded), usage)
	}))
	defer server.Close()
	spec := contextSpec(server.URL)
	spec.Workflow.Context = &overload.ContextSettings{RepoTools: true, MaxToolSteps: 4}
	spec.Workflow.Agents[0].Model.Pricing = &overload.Pricing{Input: 2, CachedInput: 2, CacheWrite: 2, Output: 10}
	entry := "Find cross-file bugs."
	spec.Workflow.Agents = append(spec.Workflow.Agents, overload.ResolvedAgent{Name: "correctness", Model: spec.Workflow.Agents[0].Model, EntryPrompt: overload.PromptTemplate{Kind: "entry", Body: entry, SHA256: overload.PromptDigest(entry)}, Scope: overload.Scope{Mode: overload.ScopeWholePR}})
	result, err := (Reviewer{}).Review(context.Background(), spec, renameRepo())
	if err != nil {
		t.Fatal(err)
	}
	if wholeCalls != 2 || !strings.Contains(toolResult, "users.fetch_user(user_id)") {
		t.Fatalf("whole-PR calls = %d, tool result seen = %v", wholeCalls, strings.Contains(toolResult, "fetch_user"))
	}
	agent := result.Metrics["routing"].(overload.Routing).Agents[1]
	// Two steps of 1,000 input x $2 + 50 output x $10 per million.
	if len(result.Findings) != 1 || agent.Calls != 2 || agent.CostUSD < 0.004999 || agent.CostUSD > 0.005001 {
		t.Fatalf("findings = %d, agent = %+v", len(result.Findings), agent)
	}
}
