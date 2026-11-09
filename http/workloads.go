package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/gateway"
	"github.com/daltoniam/overload/queue"
	"github.com/daltoniam/overload/web/templates/pages"
	"github.com/daltoniam/overload/workload"
	"github.com/jackc/pgx/v5"
)

// workloadClient fetches workload token issuers' keys; nil uses a default
// client. Tests point it at a fake issuer.
var workloadClient *http.Client

// WorkloadRuleStore manages the rules that trust automation's OIDC tokens.
type WorkloadRuleStore interface {
	ListWorkloadRules(context.Context) ([]overload.WorkloadRule, error)
	SaveWorkloadRule(context.Context, overload.WorkloadRule) error
	DeleteWorkloadRule(context.Context, string) error
}

// ReviewQueuer queues a review of a pull request by repository name.
type ReviewQueuer interface {
	QueueReviewByName(ctx context.Context, repository string, number int, again bool) (int64, error)
}

type apiKeyAuthenticator interface {
	AuthenticateAPIKey(ctx context.Context, secret string) (overload.APIKey, *overload.User, error)
}

func bearerToken(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(token)
}

// workloadModels lets workload tokens with the models scope call the
// gateway.
func workloadModels(verifier *workload.Verifier) gateway.Authenticator {
	return func(r *http.Request) (gateway.Caller, bool, error) {
		token := bearerToken(r)
		if token == "" || strings.HasPrefix(token, "ovl_") {
			return gateway.Caller{}, false, nil
		}
		identity, err := verifier.Verify(r.Context(), token)
		if errors.Is(err, workload.ErrNotWorkloadToken) {
			return gateway.Caller{}, false, nil
		}
		if err != nil {
			return gateway.Caller{}, false, err
		}
		if !identity.HasScope(overload.ScopeModels) {
			return gateway.Caller{}, false, fmt.Errorf("%w: no workload rule lets this token call models", gateway.ErrNotAllowed)
		}
		return gateway.Caller{Name: identity.Name()}, true, nil
	}
}

type reviewRequest struct {
	Repository  string `json:"repository"`
	PullRequest int    `json:"pull_request"`
	Again       bool   `json:"again"`
}

// reviewAPI queues a review for an API key or a workload token with the
// reviews scope. A workload token that names a repository may only request
// reviews of that repository.
func reviewAPI(queuer ReviewQueuer, keys apiKeyAuthenticator, verifier *workload.Verifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
			return
		}
		var request reviewRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil || !overload.ValidRepositoryName(request.Repository) || request.PullRequest < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"repository": "owner/name", "pull_request": 123}`})
			return
		}
		who, err := authorizeReview(r, keys, verifier, request.Repository)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		runID, err := queuer.QueueReviewByName(r.Context(), request.Repository, request.PullRequest, request.Again)
		if err != nil {
			message := "review not queued"
			if errors.Is(err, queue.ErrReviewNotQueued) {
				message = err.Error()
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": message})
			return
		}
		slog.Info("review requested through the API", "run_id", runID, "repository", request.Repository, "pull_request", request.PullRequest, "by", who)
		response := map[string]any{"run_id": runID}
		if base := strings.TrimRight(baseURL(), "/"); base != "" {
			response["run_url"] = fmt.Sprintf("%s/runs/%d", base, runID)
		}
		writeJSON(w, http.StatusAccepted, response)
	})
}

func authorizeReview(r *http.Request, keys apiKeyAuthenticator, verifier *workload.Verifier, repository string) (string, error) {
	token := bearerToken(r)
	switch {
	case token == "":
		return "", errors.New("send an overload API key or a workload OIDC token as a bearer token")
	case strings.HasPrefix(token, "ovl_"):
		if keys == nil {
			return "", errors.New("API keys are not available")
		}
		key, user, err := keys.AuthenticateAPIKey(r.Context(), token)
		if err != nil || !key.HasScope(overload.ScopeReviews) {
			return "", errors.New("invalid API key, or it cannot request reviews")
		}
		if user != nil && !overload.RoleAtLeast(user.Role, overload.RoleMember) {
			return "", errors.New("the key's owner cannot request reviews")
		}
		return "key:" + key.Prefix, nil
	case verifier != nil:
		identity, err := verifier.Verify(r.Context(), token)
		if err != nil {
			return "", err
		}
		if !identity.HasScope(overload.ScopeReviews) {
			return "", errors.New("no workload rule lets this token request reviews")
		}
		// A token naming a repository (every GitHub Actions token does) may
		// only request reviews of that repository.
		own, named := identity.Claims["repository"].(string)
		if (named || identity.Issuer == overload.GitHubActionsIssuer) && !strings.EqualFold(own, repository) {
			return "", errors.New("a workload token can only request reviews of its own repository")
		}
		return identity.Name(), nil
	default:
		return "", errors.New("workload tokens are not available")
	}
}

func baseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("OVERLOAD_BASE_URL")), "/")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func registerWorkloads(mux *http.ServeMux, store WorkloadRuleStore, csrf string) {
	mux.HandleFunc("GET /configure/workloads", func(w http.ResponseWriter, r *http.Request) {
		rules, err := store.ListWorkloadRules(r.Context())
		if err != nil {
			http.Error(w, "Unable to load workload rules", http.StatusInternalServerError)
			return
		}
		_ = pages.WorkloadRules(rules).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /configure/workloads/{name}", func(w http.ResponseWriter, r *http.Request) {
		rule := overload.WorkloadRule{Issuer: overload.GitHubActionsIssuer, Audience: baseURL(), Scopes: []string{overload.ScopeReviews}, Enabled: true}
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
		_ = pages.WorkloadRuleForm(rule, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /configure/workloads", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		rule := overload.WorkloadRule{Name: strings.TrimSpace(r.PostForm.Get("name")), Issuer: strings.TrimSpace(r.PostForm.Get("issuer")), Audience: strings.TrimSpace(r.PostForm.Get("audience")), Scopes: r.PostForm["scopes"], Enabled: r.PostForm.Get("enabled") == "true", Conditions: map[string]string{}}
		if existing := r.PostForm.Get("existing"); existing != "" && existing != rule.Name {
			http.Error(w, "Rule name cannot be changed", http.StatusBadRequest)
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
		if err := store.SaveWorkloadRule(r.Context(), rule); err != nil {
			http.Error(w, "Unable to save the rule", http.StatusInternalServerError)
			return
		}
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
		http.Redirect(w, r, "/configure/workloads", http.StatusSeeOther)
	})
}
