package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/gateway"
	"github.com/daltoniam/overload/queue"
	"github.com/daltoniam/overload/web/templates/pages"
	"github.com/daltoniam/overload/workload"
	"github.com/jackc/pgx/v5"
)

// WorkloadRuleStore manages the rules that trust automation's OIDC tokens.
type WorkloadRuleStore interface {
	ListWorkloadRules(context.Context) ([]overload.WorkloadRule, error)
	CreateWorkloadRule(context.Context, overload.WorkloadRule) error
	UpdateWorkloadRule(context.Context, overload.WorkloadRule) error
	DeleteWorkloadRule(context.Context, string) error
}

// ReviewQueuer queues a review of a pull request by repository name.
type ReviewQueuer interface {
	QueueReviewByName(ctx context.Context, repository string, number int, again bool) (int64, error)
}

type apiKeyAuthenticator interface {
	AuthenticateAPIKey(ctx context.Context, secret string) (overload.APIKey, *overload.User, error)
}

// workloadModels lets workload tokens with the models scope call the
// gateway.
func workloadModels(verifier *workload.Verifier) gateway.Authenticator {
	return func(r *http.Request) (gateway.Caller, bool, error) {
		token := gateway.Credential(r)
		if token == "" || strings.HasPrefix(token, "ovl_") {
			return gateway.Caller{}, false, nil
		}
		identity, err := verifier.Verify(r.Context(), token)
		switch {
		case errors.Is(err, workload.ErrNotWorkloadToken):
			return gateway.Caller{}, false, nil
		case errors.Is(err, workload.ErrUnavailable):
			return gateway.Caller{}, false, gateway.ErrCannotAuthenticate
		case err != nil:
			return gateway.Caller{}, false, err
		}
		if !identity.HasScope(overload.ScopeModels) {
			return gateway.Caller{}, false, fmt.Errorf("%w: no workload rule lets this token call models", gateway.ErrNotAllowed)
		}
		return gateway.Caller{Name: identity.Name(), Workload: identity.Label()}, true, nil
	}
}

type reviewRequest struct {
	Repository  string `json:"repository"`
	PullRequest int    `json:"pull_request"`
	Again       bool   `json:"again"`
}

// reviewCaller is who asked for a review through the API.
type reviewCaller struct {
	name string
	// repository, when set, is the only repository the caller may review.
	repository string
}

// apiError is a refused API request.
type apiError struct {
	status  int
	message string
}

func (failure *apiError) write(w http.ResponseWriter) {
	if failure.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeJSON(w, failure.status, map[string]string{"error": failure.message})
}

// rerunsPerHour bounds the forced re-reviews (`again`) one caller may
// request: each costs a full model review of an unchanged commit.
const rerunsPerHour = 10

// reviewAPI queues a review for an API key or a workload token with the
// reviews scope. A workload token that names a repository may only request
// reviews of that repository.
func reviewAPI(queuer ReviewQueuer, keys apiKeyAuthenticator, verifier *workload.Verifier, reruns *rerunLimiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
			return
		}
		caller, failure := authenticateReviewer(r, keys, verifier)
		if failure != nil {
			failure.write(w)
			return
		}
		var request reviewRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil || !overload.ValidRepositoryName(request.Repository) || request.PullRequest < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"repository": "owner/name", "pull_request": 123}`})
			return
		}
		if caller.repository != "" && !strings.EqualFold(caller.repository, request.Repository) {
			(&apiError{http.StatusForbidden, "a workload token can only request reviews of its own repository"}).write(w)
			return
		}
		if request.Again {
			if wait, ok := reruns.allow(caller.name); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("at most %d forced re-reviews an hour; push a new commit or retry later", rerunsPerHour)})
				return
			}
		}
		runID, err := queuer.QueueReviewByName(r.Context(), request.Repository, request.PullRequest, request.Again)
		if errors.Is(err, queue.ErrReviewNotQueued) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			slog.Warn("review request through the API failed", "repository", request.Repository, "pull_request", request.PullRequest, "by", caller.name, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "review not queued; retry later"})
			return
		}
		slog.Info("review requested through the API", "run_id", runID, "repository", request.Repository, "pull_request", request.PullRequest, "again", request.Again, "by", caller.name)
		response := map[string]any{"run_id": runID}
		if base := baseURL(); base != "" {
			response["run_url"] = fmt.Sprintf("%s/runs/%d", base, runID)
		}
		writeJSON(w, http.StatusAccepted, response)
	})
}

// authenticateReviewer checks the request's credential before anything in
// its body is read: 401 for a missing or invalid credential, 403 for a
// valid one that may not request reviews, 503 when it cannot be checked.
func authenticateReviewer(r *http.Request, keys apiKeyAuthenticator, verifier *workload.Verifier) (reviewCaller, *apiError) {
	token := gateway.Credential(r)
	switch {
	case token == "":
		return reviewCaller{}, &apiError{http.StatusUnauthorized, "send an overload API key or a workload OIDC token as a bearer token"}
	case strings.HasPrefix(token, "ovl_"):
		if keys == nil {
			return reviewCaller{}, &apiError{http.StatusUnauthorized, "API keys are not available"}
		}
		key, user, err := keys.AuthenticateAPIKey(r.Context(), token)
		if errors.Is(err, overload.ErrInvalidAPIKey) {
			return reviewCaller{}, &apiError{http.StatusUnauthorized, "invalid, expired or revoked API key"}
		}
		if err != nil {
			slog.Warn("review API key check failed", "error", err)
			return reviewCaller{}, &apiError{http.StatusServiceUnavailable, "unable to check the API key; retry later"}
		}
		if !key.HasScope(overload.ScopeReviews) {
			return reviewCaller{}, &apiError{http.StatusForbidden, "this API key cannot request reviews"}
		}
		if user == nil {
			return reviewCaller{name: "key:" + key.Prefix}, nil
		}
		if !overload.RoleAtLeast(user.Role, overload.RoleMember) {
			return reviewCaller{}, &apiError{http.StatusForbidden, "the key's owner cannot request reviews"}
		}
		// Named after the person, so making more keys does not raise their
		// re-review allowance.
		return reviewCaller{name: "user:" + strconv.FormatInt(user.ID, 10)}, nil
	case verifier == nil:
		return reviewCaller{}, &apiError{http.StatusUnauthorized, "workload tokens are not available"}
	}
	identity, err := verifier.Verify(r.Context(), token)
	switch {
	case errors.Is(err, workload.ErrNotWorkloadToken):
		return reviewCaller{}, &apiError{http.StatusUnauthorized, "send an overload API key or a workload OIDC token as a bearer token"}
	case errors.Is(err, workload.ErrUnavailable):
		return reviewCaller{}, &apiError{http.StatusServiceUnavailable, err.Error()}
	case err != nil:
		return reviewCaller{}, &apiError{http.StatusUnauthorized, err.Error()}
	}
	if !identity.HasScope(overload.ScopeReviews) {
		return reviewCaller{}, &apiError{http.StatusForbidden, "no workload rule lets this token request reviews"}
	}
	if identity.Repository == "" && overload.IsGitHubActionsIssuer(identity.Issuer) {
		return reviewCaller{}, &apiError{http.StatusForbidden, "the GitHub token names no repository"}
	}
	return reviewCaller{name: identity.Name(), repository: identity.Repository}, nil
}

// rerunLimiter allows each caller a number of events per window.
type rerunLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	used map[string][]time.Time
}

func newRerunLimiter(limit int, window time.Duration) *rerunLimiter {
	return &rerunLimiter{limit: limit, window: window, now: time.Now, used: map[string][]time.Time{}}
}

// allow records an event for caller if it is under the limit, or returns
// how long until it would be.
func (limiter *rerunLimiter) allow(caller string) (time.Duration, bool) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.now()
	for name, times := range limiter.used {
		kept := times[:0]
		for _, at := range times {
			if now.Sub(at) < limiter.window {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(limiter.used, name)
		} else {
			limiter.used[name] = kept
		}
	}
	times := limiter.used[caller]
	if len(times) >= limiter.limit {
		return limiter.window - now.Sub(times[0]), false
	}
	limiter.used[caller] = append(times, now)
	return 0, true
}

func baseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("OVERLOAD_BASE_URL")), "/")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// newWorkloadRule is the form's starting point for a new rule.
func newWorkloadRule() overload.WorkloadRule {
	return overload.WorkloadRule{Issuer: overload.GitHubActionsIssuer, Audience: baseURL(), Scopes: []string{overload.ScopeReviews}, Enabled: true}
}

// Rule form failures answer in plain text with a 4xx status; the UI
// middleware shows the message above the form with the submitted values
// kept (renderRetainedForm).
func registerWorkloads(mux *http.ServeMux, store WorkloadRuleStore, verifier *workload.Verifier, csrf string) {
	mux.HandleFunc("GET /configure/workloads", func(w http.ResponseWriter, r *http.Request) {
		rules, err := store.ListWorkloadRules(r.Context())
		if err != nil {
			http.Error(w, "Unable to load workload rules", http.StatusInternalServerError)
			return
		}
		_ = pages.WorkloadRules(rules).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /configure/workloads/{name}", func(w http.ResponseWriter, r *http.Request) {
		rule := newWorkloadRule()
		if r.PathValue("name") != "new" {
			rules, err := store.ListWorkloadRules(r.Context())
			if err != nil {
				http.Error(w, "Unable to load workload rules", http.StatusInternalServerError)
				return
			}
			found := false
			for _, candidate := range rules {
				if candidate.Name == r.PathValue("name") {
					rule, found = candidate, true
				}
			}
			if !found {
				http.NotFound(w, r)
				return
			}
		}
		_ = pages.WorkloadRuleForm(rule, formTokenFor(r, csrf)).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /configure/workloads", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		rule := overload.WorkloadRule{Name: strings.TrimSpace(r.PostForm.Get("name")), Issuer: strings.TrimSpace(r.PostForm.Get("issuer")), Audience: strings.TrimSpace(r.PostForm.Get("audience")), Scopes: r.PostForm["scopes"], Enabled: r.PostForm.Get("enabled") == "true", Conditions: map[string]string{}}
		existing := r.PostForm.Get("existing")
		if existing != "" && existing != rule.Name {
			http.Error(w, "Rule not saved: a rule's name cannot be changed.", http.StatusUnprocessableEntity)
			return
		}
		for _, line := range strings.Split(r.PostForm.Get("conditions"), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			claim, pattern, ok := strings.Cut(line, "=")
			if !ok {
				http.Error(w, "Rule not saved: write each condition as claim=pattern.", http.StatusUnprocessableEntity)
				return
			}
			rule.Conditions[strings.TrimSpace(claim)] = strings.TrimSpace(pattern)
		}
		if err := rule.Validate(); err != nil {
			http.Error(w, "Rule not saved: "+err.Error()+".", http.StatusUnprocessableEntity)
			return
		}
		save := store.CreateWorkloadRule
		if existing != "" {
			save = store.UpdateWorkloadRule
		}
		err := save(r.Context(), rule)
		switch {
		case errors.Is(err, overload.ErrWorkloadRuleExists):
			http.Error(w, "Rule not saved: a rule named "+rule.Name+" already exists. Choose another name, or edit that rule.", http.StatusConflict)
			return
		case errors.Is(err, pgx.ErrNoRows):
			http.NotFound(w, r)
			return
		case err != nil:
			http.Error(w, "Unable to save the rule", http.StatusInternalServerError)
			return
		}
		verifier.Forget()
		http.Redirect(w, r, "/configure/workloads", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /configure/workloads/{name}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		if err := store.DeleteWorkloadRule(r.Context(), r.PathValue("name")); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Unable to delete the rule", http.StatusInternalServerError)
			return
		}
		verifier.Forget()
		http.Redirect(w, r, "/configure/workloads", http.StatusSeeOther)
	})
}
