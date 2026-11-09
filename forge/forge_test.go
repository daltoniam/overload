package forge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/diff"
)

var (
	headSHA = strings.Repeat("b", 40)
	baseSHA = strings.Repeat("a", 40)
)

type recorded struct {
	method string
	path   string
	body   map[string]any
}

// fakeHost serves canned answers keyed by "METHOD escaped-path" and
// records every request with a body.
type fakeHost struct {
	t        *testing.T
	header   string
	token    string
	routes   map[string]func(http.ResponseWriter, *http.Request)
	mu       sync.Mutex
	requests []recorded
}

func newFakeHost(t *testing.T, header, token string) (*fakeHost, *httptest.Server) {
	host := &fakeHost{t: t, header: header, token: token, routes: map[string]func(http.ResponseWriter, *http.Request){}}
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	return host, server
}

func (host *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(host.header) != host.token {
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var body map[string]any
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		_ = json.Unmarshal(data, &body)
	}
	key := r.Method + " " + r.URL.EscapedPath()
	host.mu.Lock()
	host.requests = append(host.requests, recorded{method: r.Method, path: r.URL.EscapedPath(), body: body})
	host.mu.Unlock()
	route, ok := host.routes[key]
	if !ok {
		http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
		return
	}
	route(w, r)
}

// json answers key with value; a list route answers an empty list for
// pages after the first, as real servers do.
func (host *fakeHost) json(key string, status int, value any) {
	host.routes[key] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if page := r.URL.Query().Get("page"); page != "" && page != "1" {
			if _, isList := value.([]map[string]any); isList {
				_, _ = w.Write([]byte("[]"))
				return
			}
		}
		_ = json.NewEncoder(w).Encode(value)
	}
}

func (host *fakeHost) find(method, path string) []recorded {
	host.mu.Lock()
	defer host.mu.Unlock()
	var found []recorded
	for _, request := range host.requests {
		if request.method == method && request.path == path {
			found = append(found, request)
		}
	}
	return found
}

func gitlabClient(t *testing.T, server *httptest.Server) Client {
	client, err := New(overload.ForgeConnection{Kind: overload.ForgeGitLab, BaseURL: server.URL, Token: "glpat-test"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func forgejoClient(t *testing.T, server *httptest.Server) Client {
	client, err := New(overload.ForgeConnection{Kind: overload.ForgeForgejo, BaseURL: server.URL, Token: "fj-test"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

const gitlabProjectPath = "/api/v4/projects/group%2Fsub%2Fapi"

func TestGitLabPullRequestWithDiff(t *testing.T) {
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	host.json("GET "+gitlabProjectPath+"/merge_requests/7", 200, map[string]any{"iid": 7, "state": "opened", "sha": headSHA, "target_project_id": 42, "diff_refs": map[string]string{"base_sha": baseSHA, "head_sha": headSHA, "start_sha": baseSHA}})
	host.routes["GET "+gitlabProjectPath+"/merge_requests/7/diffs"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"old_path": "main.go", "new_path": "main.go", "diff": "@@ -1,2 +1,3 @@\n package main\n+// added\n func main() {}\n"}})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"old_path": "new.txt", "new_path": "new.txt", "new_file": true, "b_mode": "100644", "diff": "@@ -0,0 +1 @@\n+hello"},
			{"old_path": "gone.txt", "new_path": "gone.txt", "deleted_file": true, "a_mode": "100644", "diff": "@@ -1 +0,0 @@\n-bye\n"},
		})
	}
	pr, patch, err := gitlabClient(t, server).PullRequestWithDiff(context.Background(), "group/sub/api", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := overload.PullRequest{Number: 7, State: "open", HeadSHA: headSHA, BaseSHA: baseSHA, RepositoryID: 42}
	if pr != want {
		t.Fatalf("pull request = %+v, want %+v", pr, want)
	}
	files, err := diff.Parse(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "main.go" || files[0].Lines[2] != "// added" || files[1].Path != "new.txt" || files[1].Lines[1] != "hello" {
		t.Fatalf("parsed diff = %+v\n%s", files, patch)
	}
	if !strings.Contains(patch, "--- /dev/null\n+++ b/new.txt") || !strings.Contains(patch, "--- a/gone.txt\n+++ /dev/null") {
		t.Fatalf("diff headers wrong:\n%s", patch)
	}
}

func TestGitLabPostReview(t *testing.T) {
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	mr := gitlabProjectPath + "/merge_requests/7"
	host.json("GET "+mr, 200, map[string]any{"iid": 7, "state": "opened", "sha": headSHA, "diff_refs": map[string]string{"base_sha": baseSHA, "head_sha": headSHA, "start_sha": baseSHA}})
	host.json("POST "+mr+"/notes", 201, map[string]any{"id": 900})
	host.routes["POST "+mr+"/discussions"] = func(w http.ResponseWriter, r *http.Request) {
		host.mu.Lock()
		last := host.requests[len(host.requests)-1]
		host.mu.Unlock()
		position, _ := last.body["position"].(map[string]any)
		if position["new_path"] == "bad.go" {
			http.Error(w, `{"message":"400 Bad request - Note {:line_code=>[\"can't be blank\"]}"}`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"abc"}`))
	}
	host.json("POST "+mr+"/approve", 201, map[string]any{})
	findings := []overload.Finding{{Path: "main.go", Line: 2, Severity: "low", Title: "Comment", Body: "Explain why."}, {Path: "bad.go", Line: 9, Severity: "medium", Title: "Off diff", Body: "Moved."}}
	id, err := gitlabClient(t, server).PostReview(context.Background(), "group/sub/api", 7, headSHA, overload.ReviewApprove, "Summary\n\n<!-- overload-run:5 -->", findings)
	if err != nil || id != 900 {
		t.Fatalf("PostReview = %d, %v", id, err)
	}
	discussions := host.find("POST", mr+"/discussions")
	if len(discussions) != 2 {
		t.Fatalf("discussions = %d", len(discussions))
	}
	position := discussions[0].body["position"].(map[string]any)
	if position["head_sha"] != headSHA || position["base_sha"] != baseSHA || position["new_line"] != float64(2) || position["position_type"] != "text" {
		t.Fatalf("position = %+v", position)
	}
	notes := host.find("POST", mr+"/notes")
	if len(notes) != 2 || !strings.Contains(notes[1].body["body"].(string), "`bad.go:9`") {
		t.Fatalf("notes = %+v", notes)
	}
	if len(host.find("POST", mr+"/approve")) != 1 {
		t.Fatal("merge request not approved")
	}
}

func TestGitLabPostReviewOfOlderCommitUsesItsVersion(t *testing.T) {
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	mr := gitlabProjectPath + "/merge_requests/7"
	newer := strings.Repeat("c", 40)
	host.json("GET "+mr, 200, map[string]any{"iid": 7, "state": "opened", "sha": newer, "diff_refs": map[string]string{"base_sha": baseSHA, "head_sha": newer, "start_sha": baseSHA}})
	host.json("GET "+mr+"/versions", 200, []map[string]string{{"head_commit_sha": newer}, {"head_commit_sha": headSHA, "base_commit_sha": baseSHA, "start_commit_sha": strings.Repeat("d", 40)}})
	host.json("POST "+mr+"/notes", 201, map[string]any{"id": 1})
	host.json("POST "+mr+"/discussions", 201, map[string]any{})
	client := gitlabClient(t, server)
	if _, err := client.PostReview(context.Background(), "group/sub/api", 7, headSHA, overload.ReviewComment, "s", []overload.Finding{{Path: "a.go", Line: 1}}); err != nil {
		t.Fatal(err)
	}
	position := host.find("POST", mr+"/discussions")[0].body["position"].(map[string]any)
	if position["head_sha"] != headSHA || position["start_sha"] != strings.Repeat("d", 40) {
		t.Fatalf("position = %+v", position)
	}
	if _, err := client.PostReview(context.Background(), "group/sub/api", 7, strings.Repeat("e", 40), overload.ReviewComment, "s", nil); !errors.Is(err, overload.ErrReviewRejected) {
		t.Fatalf("unknown commit: %v", err)
	}
}

func TestGitLabFindReviewOnlyOwnNotes(t *testing.T) {
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	host.json("GET /api/v4/user", 200, map[string]any{"id": 3, "username": "overload-bot"})
	host.json("GET "+gitlabProjectPath+"/merge_requests/7/notes", 200, []map[string]any{
		{"id": 10, "body": "copied <!-- overload-run:5 -->", "author": map[string]any{"id": 4}},
		{"id": 11, "body": "Summary <!-- overload-run:5 -->", "author": map[string]any{"id": 3}},
	})
	id, err := gitlabClient(t, server).FindReview(context.Background(), "group/sub/api", 7, "<!-- overload-run:5 -->")
	if err != nil || id != 11 {
		t.Fatalf("FindReview = %d, %v", id, err)
	}
}

func TestGitLabSetStatus(t *testing.T) {
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	host.json("POST "+gitlabProjectPath+"/statuses/"+headSHA, 201, map[string]any{})
	client := gitlabClient(t, server)
	if err := client.SetStatus(context.Background(), "group/sub/api", headSHA, "failure", "Changes requested", "https://overload.example/runs/5"); err != nil {
		t.Fatal(err)
	}
	body := host.find("POST", gitlabProjectPath+"/statuses/"+headSHA)[0].body
	if body["state"] != "failed" || body["name"] != "overload" || body["target_url"] != "https://overload.example/runs/5" {
		t.Fatalf("status body = %+v", body)
	}
	host.json("POST "+gitlabProjectPath+"/statuses/"+headSHA, 403, map[string]any{"message": "403 Forbidden"})
	if err := client.SetStatus(context.Background(), "group/sub/api", headSHA, "success", "ok", ""); !errors.Is(err, overload.ErrStatusForbidden) {
		t.Fatalf("forbidden: %v", err)
	}
}

func TestGitLabCreateWebhookUpdatesExisting(t *testing.T) {
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	host.json("GET "+gitlabProjectPath+"/hooks", 200, []map[string]any{{"id": 8, "url": "https://overload.example/webhooks/gitlab/1"}})
	host.json("PUT "+gitlabProjectPath+"/hooks/8", 200, map[string]any{})
	if err := gitlabClient(t, server).CreateWebhook(context.Background(), "group/sub/api", "https://overload.example/webhooks/gitlab/1", "s3cret"); err != nil {
		t.Fatal(err)
	}
	updated := host.find("PUT", gitlabProjectPath+"/hooks/8")
	if len(updated) != 1 || updated[0].body["token"] != "s3cret" || updated[0].body["merge_requests_events"] != true {
		t.Fatalf("hook update = %+v", updated)
	}
}

const forgejoRepoPath = "/api/v1/repos/acme/api"

func TestForgejoPullRequestWithDiff(t *testing.T) {
	host, server := newFakeHost(t, "Authorization", "token fj-test")
	host.json("GET "+forgejoRepoPath+"/pulls/3", 200, map[string]any{"number": 3, "state": "open", "title": "WIP: refactor", "head": map[string]any{"sha": headSHA}, "base": map[string]any{"sha": baseSHA, "repo": map[string]any{"id": 77}}})
	host.routes["GET "+forgejoRepoPath+"/pulls/3.diff"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n"))
	}
	pr, patch, err := forgejoClient(t, server).PullRequestWithDiff(context.Background(), "acme/api", 3)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 3 || pr.State != "open" || !pr.Draft || pr.HeadSHA != headSHA || pr.BaseSHA != baseSHA || pr.RepositoryID != 77 || !strings.HasPrefix(patch, "diff --git") {
		t.Fatalf("pr = %+v patch = %q", pr, patch)
	}
}

func TestForgejoPostReview(t *testing.T) {
	host, server := newFakeHost(t, "Authorization", "token fj-test")
	host.json("POST "+forgejoRepoPath+"/pulls/3/reviews", 200, map[string]any{"id": 55})
	client := forgejoClient(t, server)
	id, err := client.PostReview(context.Background(), "acme/api", 3, headSHA, overload.ReviewApprove, "Summary", []overload.Finding{{Path: "x", Line: 1, Severity: "low", Title: "T", Body: "B"}})
	if err != nil || id != 55 {
		t.Fatalf("PostReview = %d, %v", id, err)
	}
	body := host.find("POST", forgejoRepoPath+"/pulls/3/reviews")[0].body
	comments := body["comments"].([]any)
	if body["event"] != "APPROVED" || body["commit_id"] != headSHA || len(comments) != 1 || comments[0].(map[string]any)["new_position"] != float64(1) {
		t.Fatalf("review body = %+v", body)
	}
	host.json("POST "+forgejoRepoPath+"/pulls/3/reviews", 422, map[string]any{"message": "approve your own pull is not allowed"})
	if _, err := client.PostReview(context.Background(), "acme/api", 3, headSHA, overload.ReviewApprove, "Summary", nil); !errors.Is(err, overload.ErrReviewRejected) {
		t.Fatalf("rejected review: %v", err)
	}
}

func TestForgejoOwnReviews(t *testing.T) {
	host, server := newFakeHost(t, "Authorization", "token fj-test")
	host.json("GET /api/v1/user", 200, map[string]any{"login": "overload"})
	host.json("GET "+forgejoRepoPath+"/pulls/3/reviews", 200, []map[string]any{
		{"id": 1, "state": "REQUEST_CHANGES", "body": "x <!-- overload-run:9 -->", "user": map[string]any{"login": "overload"}},
		{"id": 2, "state": "REQUEST_CHANGES", "user": map[string]any{"login": "someone"}},
		{"id": 3, "state": "REQUEST_CHANGES", "dismissed": true, "user": map[string]any{"login": "overload"}},
	})
	host.json("POST "+forgejoRepoPath+"/pulls/3/reviews/1/dismissals", 403, map[string]any{"message": "forbidden"})
	client := forgejoClient(t, server)
	ids, err := client.ChangesRequested(context.Background(), "acme/api", 3)
	if err != nil || len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ChangesRequested = %v, %v", ids, err)
	}
	if id, err := client.FindReview(context.Background(), "acme/api", 3, "<!-- overload-run:9 -->"); err != nil || id != 1 {
		t.Fatalf("FindReview = %d, %v", id, err)
	}
	if err := client.DismissReview(context.Background(), "acme/api", 3, 1, "done"); err != nil {
		t.Fatalf("dismiss without admin rights should only warn: %v", err)
	}
}

func TestForgejoSetStatusAndWebhook(t *testing.T) {
	host, server := newFakeHost(t, "Authorization", "token fj-test")
	host.json("POST "+forgejoRepoPath+"/statuses/"+headSHA, 201, map[string]any{})
	host.json("GET "+forgejoRepoPath+"/hooks", 200, []map[string]any{})
	host.json("POST "+forgejoRepoPath+"/hooks", 201, map[string]any{"id": 1})
	client := forgejoClient(t, server)
	if err := client.SetStatus(context.Background(), "acme/api", headSHA, "pending", "Reviewing", ""); err != nil {
		t.Fatal(err)
	}
	if body := host.find("POST", forgejoRepoPath+"/statuses/"+headSHA)[0].body; body["state"] != "pending" || body["context"] != "overload" {
		t.Fatalf("status = %+v", body)
	}
	if err := client.CreateWebhook(context.Background(), "acme/api", "https://overload.example/webhooks/forgejo/2", "s3cret"); err != nil {
		t.Fatal(err)
	}
	hook := host.find("POST", forgejoRepoPath+"/hooks")[0].body
	if hook["type"] != "gitea" || hook["config"].(map[string]any)["secret"] != "s3cret" {
		t.Fatalf("hook = %+v", hook)
	}
}

func TestDownloadHeadRefusesOtherHosts(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "" {
			t.Error("token forwarded to another host")
		}
		_, _ = w.Write([]byte("archive"))
	}))
	defer other.Close()
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	host.routes["GET "+gitlabProjectPath+"/repository/archive.tar.gz"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/archive", http.StatusFound)
	}
	var archive bytes.Buffer
	if err := gitlabClient(t, server).DownloadHead(context.Background(), "group/sub/api", headSHA, &archive); err == nil {
		t.Fatal("followed a redirect to another host")
	}
}

func TestNewValidatesConnection(t *testing.T) {
	tests := []overload.ForgeConnection{
		{Kind: "bitbucket", BaseURL: "https://example.com", Token: "t"},
		{Kind: overload.ForgeGitLab, BaseURL: "http://gitlab.example.com", Token: "t"},
		{Kind: overload.ForgeGitLab, BaseURL: "https://gitlab.example.com"},
	}
	for _, connection := range tests {
		if _, err := New(connection); err == nil {
			t.Errorf("New(%+v) accepted", connection)
		}
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerify(t *testing.T) {
	body := []byte(`{"a":1}`)
	tests := []struct {
		name   string
		kind   string
		header http.Header
		want   bool
	}{
		{"gitlab token", overload.ForgeGitLab, http.Header{"X-Gitlab-Token": {"s3cret"}}, true},
		{"gitlab wrong token", overload.ForgeGitLab, http.Header{"X-Gitlab-Token": {"nope"}}, false},
		{"forgejo signature", overload.ForgeForgejo, http.Header{"X-Forgejo-Signature": {sign("s3cret", body)}}, true},
		{"gitea signature", overload.ForgeForgejo, http.Header{"X-Gitea-Signature": {sign("s3cret", body)}}, true},
		{"forgejo bad signature", overload.ForgeForgejo, http.Header{"X-Forgejo-Signature": {sign("other", body)}}, false},
		{"forgejo missing", overload.ForgeForgejo, http.Header{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Verify(test.kind, test.header, body, "s3cret"); got != test.want {
				t.Fatalf("Verify = %v", got)
			}
		})
	}
	if Verify(overload.ForgeGitLab, http.Header{"X-Gitlab-Token": {""}}, body, "") {
		t.Fatal("empty secret verified")
	}
}

func TestParseGitLab(t *testing.T) {
	payload := func(action, state string, draft bool, oldrev string, changes string) []byte {
		return []byte(`{"object_kind":"merge_request","project":{"id":42,"path_with_namespace":"group/sub/api"},"object_attributes":{"iid":7,"action":"` + action + `","state":"` + state + `","draft":` + map[bool]string{true: "true", false: "false"}[draft] + `,"oldrev":"` + oldrev + `","last_commit":{"id":"` + headSHA + `"}},"changes":` + changes + `}`)
	}
	tests := []struct {
		name     string
		body     []byte
		action   string
		eligible bool
	}{
		{"open", payload("open", "opened", false, "", "{}"), "opened", true},
		{"open draft", payload("open", "opened", true, "", "{}"), "opened", false},
		{"push", payload("update", "opened", false, baseSHA, "{}"), "synchronize", true},
		{"title edit", payload("update", "opened", false, "", `{"title":{}}`), "update", false},
		{"marked ready", payload("update", "opened", false, "", `{"draft":{"previous":true,"current":false}}`), "ready_for_review", true},
		{"reopen", payload("reopen", "opened", false, "", "{}"), "reopened", true},
		{"merge", payload("merge", "merged", false, "", "{}"), "merge", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, err := Parse(overload.ForgeGitLab, "Merge Request Hook", test.body)
			if err != nil {
				t.Fatal(err)
			}
			if event.Action != test.action || event.Eligible != test.eligible || event.Repository != "group/sub/api" || event.RepositoryID != 42 || event.Number != 7 || event.HeadSHA != headSHA {
				t.Fatalf("event = %+v", event)
			}
		})
	}
	if _, err := Parse(overload.ForgeGitLab, "Push Hook", []byte(`{}`)); !errors.Is(err, ErrNotPullRequest) {
		t.Fatalf("push hook: %v", err)
	}
}

func TestParseForgejo(t *testing.T) {
	payload := func(action, title, changes string) []byte {
		return []byte(`{"action":"` + action + `","number":3,"pull_request":{"state":"open","title":"` + title + `","head":{"sha":"` + headSHA + `"},"base":{"sha":"` + baseSHA + `"}},"repository":{"id":77,"full_name":"acme/api"},"changes":` + changes + `}`)
	}
	tests := []struct {
		name     string
		body     []byte
		action   string
		eligible bool
	}{
		{"opened", payload("opened", "Fix", "{}"), "opened", true},
		{"opened wip", payload("opened", "WIP: Fix", "{}"), "opened", false},
		{"synchronized", payload("synchronized", "Fix", "{}"), "synchronize", true},
		{"wip removed", payload("edited", "Fix", `{"title":{"from":"WIP: Fix"}}`), "ready_for_review", true},
		{"other edit", payload("edited", "Fix", `{"title":{"from":"Fx"}}`), "edited", false},
		{"closed", payload("closed", "Fix", "{}"), "closed", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, err := Parse(overload.ForgeForgejo, "pull_request", test.body)
			if err != nil {
				t.Fatal(err)
			}
			if event.Action != test.action || event.Eligible != test.eligible || event.Repository != "acme/api" || event.RepositoryID != 77 || event.BaseSHA != baseSHA {
				t.Fatalf("event = %+v", event)
			}
		})
	}
	id, event := Delivery(overload.ForgeForgejo, http.Header{"X-Gitea-Delivery": {"d1"}, "X-Gitea-Event": {"pull_request"}})
	if id != "d1" || event != "pull_request" {
		t.Fatalf("Delivery = %q %q", id, event)
	}
}

func TestGitLabApprovesOnlyTheReviewedCommit(t *testing.T) {
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	mr := gitlabProjectPath + "/merge_requests/7"
	newer := strings.Repeat("c", 40)
	host.json("GET "+mr, 200, map[string]any{"iid": 7, "state": "opened", "sha": newer, "diff_refs": map[string]string{"base_sha": baseSHA, "head_sha": newer, "start_sha": baseSHA}})
	host.json("GET "+mr+"/versions", 200, []map[string]any{{"head_commit_sha": headSHA, "base_commit_sha": baseSHA, "start_commit_sha": baseSHA}})
	host.json("POST "+mr+"/notes", 201, map[string]any{"id": 1})
	host.json("POST "+mr+"/approve", 201, map[string]any{})
	client := gitlabClient(t, server)
	if _, err := client.PostReview(context.Background(), "group/sub/api", 7, headSHA, overload.ReviewApprove, "s", nil); err != nil {
		t.Fatal(err)
	}
	if approvals := host.find("POST", mr+"/approve"); len(approvals) != 0 {
		t.Fatalf("approved a head that was not reviewed: %+v", approvals)
	}
	host.json("GET "+mr, 200, map[string]any{"iid": 7, "state": "opened", "sha": headSHA, "diff_refs": map[string]string{"base_sha": baseSHA, "head_sha": headSHA, "start_sha": baseSHA}})
	if _, err := client.PostReview(context.Background(), "group/sub/api", 7, headSHA, overload.ReviewApprove, "s", nil); err != nil {
		t.Fatal(err)
	}
	if approvals := host.find("POST", mr+"/approve"); len(approvals) != 1 || approvals[0].body["sha"] != headSHA {
		t.Fatalf("approvals = %+v", approvals)
	}
}

func TestGitLabDiffMatchesItsHead(t *testing.T) {
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	mr := gitlabProjectPath + "/merge_requests/7"
	newer := strings.Repeat("c", 40)
	reads := 0
	host.routes["GET "+mr] = func(w http.ResponseWriter, _ *http.Request) {
		reads++
		sha := headSHA
		if reads > 1 {
			// A push lands while the first diff is read.
			sha = newer
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7, "state": "opened", "sha": sha, "diff_refs": map[string]string{"base_sha": baseSHA, "head_sha": sha}})
	}
	diffs := 0
	host.routes["GET "+mr+"/diffs"] = func(w http.ResponseWriter, _ *http.Request) {
		diffs++
		_ = json.NewEncoder(w).Encode([]map[string]any{{"old_path": "a.go", "new_path": "a.go", "diff": fmt.Sprintf("@@ -1 +1 @@\n-x\n+read%d\n", diffs)}})
	}
	pr, patch, err := gitlabClient(t, server).PullRequestWithDiff(context.Background(), "group/sub/api", 7)
	if err != nil {
		t.Fatal(err)
	}
	if pr.HeadSHA != newer || !strings.Contains(patch, "+read2") || diffs != 2 {
		t.Fatalf("head %s with diff %q after %d reads", pr.HeadSHA, patch, diffs)
	}
}

func TestWebhooksFoundOnLaterPages(t *testing.T) {
	gitlabHost, gitlabServer := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	url := "https://overload.example/webhooks/gitlab/1"
	gitlabHost.routes["GET "+gitlabProjectPath+"/hooks"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(`[{"id":1,"url":"https://other.example"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":9,"url":"` + url + `"}]`))
	}
	gitlabHost.json("PUT "+gitlabProjectPath+"/hooks/9", 200, map[string]any{})
	if err := gitlabClient(t, gitlabServer).CreateWebhook(context.Background(), "group/sub/api", url, "s"); err != nil {
		t.Fatal(err)
	}
	if len(gitlabHost.find("PUT", gitlabProjectPath+"/hooks/9")) != 1 || len(gitlabHost.find("POST", gitlabProjectPath+"/hooks")) != 0 {
		t.Fatal("GitLab hook on page 2 not updated")
	}
	forgejoHost, forgejoServer := newFakeHost(t, "Authorization", "token fj-test")
	forgejoURL := "https://overload.example/webhooks/forgejo/2"
	forgejoHost.routes["GET "+forgejoRepoPath+"/hooks"] = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(`[{"id":1,"config":{"url":"https://other.example"}}]`))
		case "2":
			_, _ = w.Write([]byte(`[{"id":5,"config":{"url":"` + forgejoURL + `"}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}
	forgejoHost.json("PATCH "+forgejoRepoPath+"/hooks/5", 200, map[string]any{})
	if err := forgejoClient(t, forgejoServer).CreateWebhook(context.Background(), "acme/api", forgejoURL, "s"); err != nil {
		t.Fatal(err)
	}
	if len(forgejoHost.find("PATCH", forgejoRepoPath+"/hooks/5")) != 1 || len(forgejoHost.find("POST", forgejoRepoPath+"/hooks")) != 0 {
		t.Fatal("Forgejo hook on page 2 not updated")
	}
}

func TestForgejoEndlessReviewHistoryIsAnError(t *testing.T) {
	host, server := newFakeHost(t, "Authorization", "token fj-test")
	host.json("GET /api/v1/user", 200, map[string]any{"login": "overload"})
	host.routes["GET "+forgejoRepoPath+"/pulls/3/reviews"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"state":"COMMENT","user":{"login":"someone"}}]`))
	}
	if _, err := forgejoClient(t, server).FindReview(context.Background(), "acme/api", 3, "<!-- overload-run:1 -->"); !errors.Is(err, errTooManyPages) {
		t.Fatalf("truncated history treated as complete: %v", err)
	}
}

func TestForgejoDiffMatchesItsHead(t *testing.T) {
	host, server := newFakeHost(t, "Authorization", "token fj-test")
	newer := strings.Repeat("c", 40)
	reads, diffs := 0, 0
	host.routes["GET "+forgejoRepoPath+"/pulls/3"] = func(w http.ResponseWriter, _ *http.Request) {
		reads++
		sha := headSHA
		if reads > 1 {
			sha = newer
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 3, "state": "open", "head": map[string]any{"sha": sha}, "base": map[string]any{"sha": baseSHA}})
	}
	host.routes["GET "+forgejoRepoPath+"/pulls/3.diff"] = func(w http.ResponseWriter, _ *http.Request) {
		diffs++
		_, _ = fmt.Fprintf(w, "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+read%d\n", diffs)
	}
	pr, patch, err := forgejoClient(t, server).PullRequestWithDiff(context.Background(), "acme/api", 3)
	if err != nil || pr.HeadSHA != newer || !strings.Contains(patch, "+read2") {
		t.Fatalf("head %s with diff %q: %v", pr.HeadSHA, patch, err)
	}
}

func TestGitLabStaleReviewKeepsANewerApproval(t *testing.T) {
	host, server := newFakeHost(t, "PRIVATE-TOKEN", "glpat-test")
	mr := gitlabProjectPath + "/merge_requests/7"
	newer := strings.Repeat("c", 40)
	host.json("GET "+mr, 200, map[string]any{"iid": 7, "state": "opened", "sha": newer, "diff_refs": map[string]string{"base_sha": baseSHA, "head_sha": newer, "start_sha": baseSHA}})
	host.json("GET "+mr+"/versions", 200, []map[string]any{{"head_commit_sha": headSHA, "base_commit_sha": baseSHA, "start_commit_sha": baseSHA}})
	host.json("POST "+mr+"/notes", 201, map[string]any{"id": 1})
	host.json("POST "+mr+"/unapprove", 201, map[string]any{})
	if _, err := gitlabClient(t, server).PostReview(context.Background(), "group/sub/api", 7, headSHA, overload.ReviewRequestChanges, "s", nil); err != nil {
		t.Fatal(err)
	}
	if len(host.find("POST", mr+"/unapprove")) != 0 {
		t.Fatal("a review of an older commit withdrew the approval")
	}
}

func TestForgejoReturnsTheLatestState(t *testing.T) {
	host, server := newFakeHost(t, "Authorization", "token fj-test")
	reads := 0
	host.routes["GET "+forgejoRepoPath+"/pulls/3"] = func(w http.ResponseWriter, _ *http.Request) {
		reads++
		state := "open"
		if reads > 1 {
			state = "closed"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 3, "state": state, "head": map[string]any{"sha": headSHA}, "base": map[string]any{"sha": baseSHA}})
	}
	host.routes["GET "+forgejoRepoPath+"/pulls/3.diff"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("diff --git a/x b/x\n"))
	}
	if pr, _, err := forgejoClient(t, server).PullRequestWithDiff(context.Background(), "acme/api", 3); err != nil || pr.State != "closed" {
		t.Fatalf("state = %q %v", pr.State, err)
	}
}
