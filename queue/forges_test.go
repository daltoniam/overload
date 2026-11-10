package queue

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
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// fakeForgejo answers the Forgejo API calls posting makes and records
// their bodies by path.
type fakeForgejo struct {
	mu     sync.Mutex
	bodies map[string][]map[string]any
}

func (host *fakeForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "token fj-token" {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var body map[string]any
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		_ = json.Unmarshal(data, &body)
	}
	host.mu.Lock()
	host.bodies[r.Method+" "+r.URL.Path] = append(host.bodies[r.Method+" "+r.URL.Path], body)
	host.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
		_, _ = w.Write([]byte(`{"login":"overload"}`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
		_, _ = w.Write([]byte(`[]`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
		_, _ = w.Write([]byte(`{"id":77}`))
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/statuses/"):
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	default:
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}
}

func (host *fakeForgejo) calls(key string) []map[string]any {
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.bodies[key]
}

func TestForgeReviewIsIngestedAndPosted(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	t.Setenv("OVERLOAD_ENABLE_POSTING", "1")
	t.Setenv("OVERLOAD_BASE_URL", "https://overload.example")
	ctx := context.Background()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	host := &fakeForgejo{bodies: map[string][]map[string]any{}}
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)

	id := time.Now().UnixNano()
	name := fmt.Sprintf("forge-%d", id)
	repo := "acme/" + name
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE repository_id IN (SELECT id FROM repositories WHERE full_name=$1))`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM findings WHERE run_id IN (SELECT id FROM runs WHERE repository_id IN (SELECT id FROM repositories WHERE full_name=$1))`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE repository_full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id IN (SELECT id FROM repositories WHERE full_name=$1)`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM trigger_bindings WHERE repository_full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM forge_connections WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
	})
	connection, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeForgejo, BaseURL: server.URL, Token: "fj-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: "http://127.0.0.1:1/v1", Model: "m", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Model: name, Prompt: "Review.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "pr_review", Agents: []string{name}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRepository(ctx, name, repo, true, false); err != nil {
		t.Fatal(err)
	}
	// The same name on GitHub is a different repository.
	if err := store.SaveRepository(ctx, "", repo, false, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBinding(ctx, overload.TriggerBinding{Source: overload.ForgeForgejo, Event: "pull_request", Action: "opened", Repository: repo, Workflow: name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("b", 40)
	delivery := postgres.PullRequestDelivery{DeliveryID: name, Action: "opened", RepoName: repo, RepoID: id % 1_000_000_000, ForgeID: connection.ID, Forge: overload.ForgeForgejo, PR: 4, HeadSHA: head, BaseSHA: strings.Repeat("a", 40), Eligible: true, Payload: []byte(`{"n":"` + name + `"}`)}
	if queued, err := store.IngestPR(ctx, client, delivery); err != nil || !queued {
		t.Fatalf("ingest: %v %v", queued, err)
	}
	outcome, reason, runID, err := store.DeliveryOutcome(ctx, overload.ForgeForgejo, name)
	if err != nil || outcome != "queued" || runID == 0 {
		t.Fatalf("outcome=%q reason=%q run=%d err=%v", outcome, reason, runID, err)
	}
	var forgeID, forgeRepoID int64
	if err := store.Pool.QueryRow(ctx, `SELECT r.forge_id, repo.forge_repo_id FROM runs r JOIN repositories repo ON repo.id = r.repository_id WHERE r.id=$1`, runID).Scan(&forgeID, &forgeRepoID); err != nil || forgeID != connection.ID || forgeRepoID != delivery.RepoID {
		t.Fatalf("run forge=%d repo id=%d err=%v", forgeID, forgeRepoID, err)
	}

	finding := overload.Finding{Path: "main.go", Line: 3, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Nil map", Body: "Initialize it.", Confidence: 0.9, Evidence: "m[k] = v"}
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET status='completed', post_status='queued' WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO findings (run_id,path,line,side,severity,category,title,body,confidence,evidence,status,fingerprint) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending',$11)`, runID, finding.Path, finding.Line, finding.Side, finding.Severity, finding.Category, finding.Title, finding.Body, finding.Confidence, finding.Evidence, overload.FindingFingerprint(repo, 4, finding)); err != nil {
		t.Fatal(err)
	}
	worker := &PostWorker{Store: store}
	if err := worker.Work(ctx, &river.Job[postgres.PostReviewArgs]{Args: postgres.PostReviewArgs{RunID: runID}}); err != nil {
		t.Fatal(err)
	}
	reviews := host.calls("POST /api/v1/repos/acme/" + name + "/pulls/4/reviews")
	if len(reviews) != 1 || reviews[0]["commit_id"] != head || !strings.Contains(reviews[0]["body"].(string), fmt.Sprintf("overload-run:%d", runID)) {
		t.Fatalf("reviews = %+v", reviews)
	}
	comments := reviews[0]["comments"].([]any)
	if len(comments) != 1 || comments[0].(map[string]any)["path"] != "main.go" {
		t.Fatalf("comments = %+v", comments)
	}
	statuses := host.calls("POST /api/v1/repos/acme/" + name + "/statuses/" + head)
	if len(statuses) == 0 || statuses[len(statuses)-1]["target_url"] != fmt.Sprintf("https://overload.example/runs/%d", runID) {
		t.Fatalf("statuses = %+v", statuses)
	}
	var postStatus string
	var reviewID int64
	if err := store.Pool.QueryRow(ctx, `SELECT post_status, github_review_id FROM runs WHERE id=$1`, runID).Scan(&postStatus, &reviewID); err != nil || postStatus != "posted" || reviewID != 77 {
		t.Fatalf("post status=%q review=%d err=%v", postStatus, reviewID, err)
	}
}

func TestForgeBudgetsAndSkipStatuses(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	host := &fakeForgejo{bodies: map[string][]map[string]any{}}
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	id := time.Now().UnixNano()
	name := fmt.Sprintf("forge-budget-%d", id)
	repo := "acme/" + name
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE repository_full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM runs WHERE repository_id IN (SELECT id FROM repositories WHERE full_name=$1)`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM budgets WHERE repository_id IN (SELECT id FROM repositories WHERE full_name=$1)`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM trigger_bindings WHERE repository_full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM repositories WHERE full_name=$1`, repo)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM forge_connections WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM workflows WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, "agent:"+name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM model_profiles WHERE name=$1`, name)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM river_job WHERE kind='skip_status' AND args->>'repository'=$1`, repo)
	})
	connection, err := store.SaveForge(ctx, overload.ForgeConnection{Name: name, Kind: overload.ForgeForgejo, BaseURL: server.URL, Token: "fj-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReviewSettings(ctx, overload.ReviewSettings{Name: name, Provider: "openaicompat", BaseURL: "http://127.0.0.1:1/v1", Model: "m", PromptProfile: "context"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, overload.AgentDefinition{Name: name, Model: name, Prompt: "Review.", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkflow(ctx, overload.Workflow{Name: name, Kind: "pr_review", Agents: []string{name}, Enabled: true, ReviewPolicy: overload.ReviewPolicy{MaxRunCostUSD: 2}}); err != nil {
		t.Fatal(err)
	}
	// The same path on GitHub and on the Forgejo connection.
	if err := store.SaveRepository(ctx, name, repo, true, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRepository(ctx, "", repo, true, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBinding(ctx, overload.TriggerBinding{Source: overload.ForgeForgejo, Event: "pull_request", Action: "opened", Repository: repo, Workflow: name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// The budget is set on the Forgejo repository by its ref.
	monthly := 1.0
	if err := store.SaveBudget(ctx, postgres.Budget{Repository: name + ":" + repo, MonthlyUSD: &monthly}); err != nil {
		t.Fatal(err)
	}
	budgets, err := store.Budgets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, budget := range budgets {
		listed = listed || budget.Repository == name+":"+repo
	}
	if !listed {
		t.Fatalf("budget not listed by its ref: %+v", budgets)
	}
	repos, err := store.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range repos {
		if candidate.FullName != repo {
			continue
		}
		check, err := store.CheckBudget(ctx, candidate.ID, 2)
		if err != nil || check.Exceeded != (candidate.ForgeID != 0) {
			t.Fatalf("%s: check = %+v %v", candidate.Ref(), check, err)
		}
	}
	client, err := river.NewClient(riverpgxv5.New(store.Pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("c", 40)
	delivery := postgres.PullRequestDelivery{DeliveryID: name, Action: "opened", RepoName: repo, RepoID: id % 1_000_000_000, ForgeID: connection.ID, Forge: overload.ForgeForgejo, PR: 4, HeadSHA: head, BaseSHA: strings.Repeat("a", 40), Eligible: true, Payload: []byte(`{"n":"` + name + `"}`)}
	if _, err := store.IngestPR(ctx, client, delivery); err != nil {
		t.Fatal(err)
	}
	if outcome, reason, _, err := store.DeliveryOutcome(ctx, overload.ForgeForgejo, name); err != nil || outcome != "skipped" || !strings.HasPrefix(reason, postgres.SkipBudgetExhausted) {
		t.Fatalf("outcome=%q reason=%q err=%v", outcome, reason, err)
	}
	var data []byte
	if err := store.Pool.QueryRow(ctx, `SELECT args FROM river_job WHERE kind='skip_status' AND args->>'repository'=$1 ORDER BY id DESC LIMIT 1`, repo).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var args postgres.SkipStatusArgs
	if err := json.Unmarshal(data, &args); err != nil || args.ForgeID != connection.ID || args.SHA != head {
		t.Fatalf("skip status job = %+v %v", args, err)
	}
	// The skip status is set through the Forgejo connection.
	if err := (&SkipStatusWorker{Store: store}).Work(ctx, &river.Job[postgres.SkipStatusArgs]{Args: args}); err != nil {
		t.Fatal(err)
	}
	statuses := host.calls("POST /api/v1/repos/acme/" + name + "/statuses/" + head)
	if len(statuses) != 1 || statuses[0]["state"] != "success" || !strings.Contains(fmt.Sprint(statuses[0]["description"]), "budget") {
		t.Fatalf("statuses = %+v", statuses)
	}
}
