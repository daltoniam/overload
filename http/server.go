package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/assets"
	"github.com/daltoniam/overload/web/templates/layouts"
	"github.com/daltoniam/overload/web/templates/pages"
)

type RunReader interface {
	ListRuns(context.Context) ([]overload.Run, error)
	GetRun(context.Context, int64) (overload.Run, error)
	RecordDelivery(context.Context, string, string, string, string, []byte) (bool, error)
	ListRunEvents(context.Context, int64) ([]postgres.RunEvent, error)
	ListDeliveries(context.Context) ([]postgres.Delivery, error)
	ListFindings(context.Context, int64) ([]overload.Finding, error)
	ReadJobOutput(context.Context, int64) (postgres.JobOutput, error)
	ReadArtifact(context.Context, int64, string) ([]byte, error)
	ListReviewSettings(context.Context) ([]overload.ReviewSettings, error)
	SaveReviewSettings(context.Context, overload.ReviewSettings) error
}

type PRIngest func(context.Context, string, string, []byte, github.PullRequestEvent) (bool, error)

func Handler(reader RunReader, ingest ...PRIngest) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", assets.Handler()))
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic("unable to initialize settings form")
	}
	csrf := hex.EncodeToString(nonce[:])
	if config, ok := reader.(ConfigurationStore); ok {
		registerConfiguration(mux, config, csrf)
	}
	appStore, hasAppStore := reader.(GitHubAppStore)
	if hasAppStore {
		registerGitHubSetup(mux, appStore, csrf)
	}
	if tools, ok := reader.(ToolServerStore); ok {
		registerTools(mux, tools, csrf)
	}
	if runner, ok := reader.(ScheduleRunner); ok {
		registerScheduleRunner(mux, runner, csrf)
	}
	if requester, ok := reader.(ReviewRequester); ok {
		registerReviewRequests(mux, requester, csrf)
	}
	if provider, ok := reader.(TunnelProvider); ok && provider.Tunnel() != nil {
		registerTunnel(mux, provider.Tunnel(), csrf)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		runs, err := reader.ListRuns(r.Context())
		if err != nil {
			http.Error(w, "Unable to load runs", http.StatusInternalServerError)
			return
		}
		var statistics postgres.DashboardStats
		if source, ok := reader.(interface {
			DashboardStatistics(context.Context, time.Time) (postgres.DashboardStats, error)
		}); ok {
			statistics, err = source.DashboardStatistics(r.Context(), time.Now())
			if err != nil {
				http.Error(w, "Unable to load dashboard statistics", http.StatusInternalServerError)
				return
			}
		}
		_ = pages.Dashboard(runs, statistics).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /runs", func(w http.ResponseWriter, r *http.Request) {
		state := parseUI(r)
		var runs []overload.Run
		var err error
		if search, ok := reader.(searchableReader); ok {
			runs, state.Total, err = search.SearchRuns(r.Context(), postgres.ListFilter{Query: state.Query, Kind: state.Kind, Status: state.Status, Page: state.Page})
			w.Header().Set("X-UI-Total", strconv.Itoa(state.Total))
		} else {
			runs, err = reader.ListRuns(r.Context())
		}
		if err != nil {
			http.Error(w, "Unable to load runs", http.StatusInternalServerError)
			return
		}
		_ = pages.Runs(runs).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.NotFound(w, r)
			return
		}
		run, err := reader.GetRun(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		events, err := reader.ListRunEvents(r.Context(), id)
		if err != nil {
			http.Error(w, "Unable to load timeline", http.StatusInternalServerError)
			return
		}
		var findings []overload.Finding
		var output postgres.JobOutput
		var routing overload.Routing
		if run.Kind == "scheduled_prompt" {
			output, err = reader.ReadJobOutput(r.Context(), id)
		} else {
			findings, err = reader.ListFindings(r.Context(), id)
			if routingReader, ok := reader.(RunRoutingReader); ok && err == nil {
				routing, err = routingReader.ReadRunRouting(r.Context(), id)
			}
		}
		if err != nil {
			http.Error(w, "Unable to load run results", http.StatusInternalServerError)
			return
		}
		nonce := ""
		if _, ok := reader.(ReviewRequester); ok {
			nonce = csrf
		}
		_ = pages.RunDetail(run, events, findings, output, routing, nonce).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /runs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.NotFound(w, r)
			return
		}
		run, err := reader.GetRun(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		events, err := reader.ListRunEvents(r.Context(), id)
		if err != nil {
			http.Error(w, "Unable to refresh events", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = pages.RunEvents(run, events).Render(pages.WithUI(r.Context(), parseUI(r)), w)
	})
	mux.HandleFunc("GET /runs/{id}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		name := r.PathValue("name")
		if err != nil || id < 1 || (name != "spec.json" && name != "result.json") {
			http.NotFound(w, r)
			return
		}
		content, err := reader.ReadArtifact(r.Context(), id, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", "attachment; filename="+name)
		_, _ = w.Write(content)
	})
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		settings, err := reader.ListReviewSettings(r.Context())
		if err != nil {
			http.Error(w, "Unable to load settings", http.StatusInternalServerError)
			return
		}
		refused := map[string]string{}
		if policies, ok := reader.(PolicyStore); ok {
			policy, err := policies.LoadModelPolicy(r.Context())
			if err != nil {
				http.Error(w, "Unable to load the model policy", http.StatusInternalServerError)
				return
			}
			for _, setting := range settings {
				if decision := policy.Check(setting.Catalog, setting.Model); !decision.Allowed {
					refused[setting.Name] = strings.Join(decision.Reasons, "; ")
				}
			}
		}
		_ = pages.Models(settings, refused).Render(r.Context(), w)
	})
	registerModels(mux, reader, csrf)
	if policies, ok := reader.(PolicyStore); ok {
		registerCatalog(mux, policies, csrf)
	}
	mux.HandleFunc("GET /webhooks", func(w http.ResponseWriter, r *http.Request) {
		state := parseUI(r)
		var deliveries []postgres.Delivery
		var err error
		if search, ok := reader.(searchableReader); ok {
			deliveries, state.Total, err = search.SearchDeliveries(r.Context(), postgres.ListFilter{Query: state.Query, Kind: state.Kind, Status: state.Status, Page: state.Page})
			w.Header().Set("X-UI-Total", strconv.Itoa(state.Total))
		} else {
			deliveries, err = reader.ListDeliveries(r.Context())
		}
		if err != nil {
			http.Error(w, "Unable to load webhooks", http.StatusInternalServerError)
			return
		}
		_ = pages.Webhooks(deliveries).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /webhooks/github", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		if !verifyWebhook(r, reader, body) {
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}
		deliveryID := r.Header.Get("X-GitHub-Delivery")
		if deliveryID == "" {
			http.Error(w, "Missing delivery ID", http.StatusBadRequest)
			return
		}
		event := r.Header.Get("X-GitHub-Event")
		var action, repoName string
		if (event == "installation" || event == "installation_repositories") && hasAppStore {
			parsed, err := github.ParseInstallation(body)
			if err != nil {
				http.Error(w, "Invalid installation event", http.StatusBadRequest)
				return
			}
			if _, err := appStore.ApplyInstallationEvent(r.Context(), deliveryID, event, body, parsed); err != nil {
				http.Error(w, "Unable to apply installation event", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if event == "pull_request" {
			parsed, err := github.ParsePullRequest(body)
			if err != nil {
				http.Error(w, "Invalid PR event", http.StatusBadRequest)
				return
			}
			action, repoName = parsed.Action, parsed.Repository.FullName
			if len(ingest) > 0 {
				_, err = ingest[0](r.Context(), deliveryID, action, body, parsed)
				if err != nil {
					http.Error(w, "Unable to ingest delivery", http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusAccepted)
				return
			}
		}
		_, err = reader.RecordDelivery(r.Context(), deliveryID, event, action, repoName, body)
		if err != nil {
			http.Error(w, "Unable to record delivery", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	settings, authErr := oidcFromEnv()
	var auth *oidcAuth
	if settings != nil {
		if sessions, ok := reader.(SessionStore); ok {
			auth = &oidcAuth{settings: settings, store: sessions, client: &http.Client{Timeout: 10 * time.Second}}
			registerAuth(mux, auth, csrf)
		} else {
			authErr = errors.New("OIDC sign-in needs the database store")
		}
	}
	return secure(uiMiddleware(mux, csrf), auth, authErr, csrf)
}

// publicPaths answer without signing in: health checks, signed webhooks
// and the sign-in flow itself.
var publicPaths = map[string]bool{"/healthz": true, "/webhooks/github": true, "/auth/login": true, "/auth/callback": true, "/auth/signed-out": true}

func secure(next http.Handler, auth *oidcAuth, authErr error, csrf string) http.Handler {
	user := os.Getenv("OVERLOAD_UI_USER")
	password := os.Getenv("OVERLOAD_UI_PASSWORD")
	insecure := os.Getenv("OVERLOAD_UI_INSECURE") == "1"
	hosts := newHostPolicy(os.Getenv("OVERLOAD_BASE_URL"))
	access, accessErr := accessFromEnv()
	basicAuth := func(r *http.Request) bool {
		givenUser, givenPassword, ok := r.BasicAuth()
		return ok && user != "" && password != "" && subtle.ConstantTimeCompare([]byte(givenUser), []byte(user)) == 1 && subtle.ConstantTimeCompare([]byte(givenPassword), []byte(password)) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' "+themePolicy+"; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/healthz" || r.URL.Path == "/webhooks/github" {
			next.ServeHTTP(w, r)
			return
		}
		if !hosts.allows(r.Host) {
			http.Error(w, "Unknown host; set OVERLOAD_BASE_URL to the address you use", http.StatusMisdirectedRequest)
			return
		}
		if accessErr != nil {
			http.Error(w, "Cloudflare Access is misconfigured", http.StatusInternalServerError)
			return
		}
		if authErr != nil {
			http.Error(w, "OIDC sign-in is misconfigured", http.StatusInternalServerError)
			return
		}
		who := identity{role: overload.RoleAdmin}
		switch {
		case access != nil:
			if _, err := access.verify(r.Context(), r.Header.Get(accessHeader)); err != nil {
				http.Error(w, "Sign in through Cloudflare Access", http.StatusForbidden)
				return
			}
		case auth != nil && publicPaths[r.URL.Path]:
			who = identity{}
		case auth != nil:
			if signedIn, ok := auth.sessionUser(r); ok {
				who = identity{user: &signedIn, role: signedIn.Role}
			} else if basicAuth(r) {
				who.breakGlass = true
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					slog.Warn("break-glass change", "method", r.Method, "path", r.URL.Path)
				}
			} else {
				denySignedOut(w, r, user != "" && password != "")
				return
			}
		case !insecure:
			if !basicAuth(r) {
				w.Header().Set("WWW-Authenticate", `Basic realm="overload"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if who.role != "" && !allowed(who.role, r) {
			http.Error(w, "Your role ("+who.role+") cannot do this. Ask an administrator.", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) {
				http.Error(w, "Invalid origin", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		}
		ctx, cancel := context.WithTimeout(withIdentity(r.Context(), who), 10*time.Second)
		defer cancel()
		if who.user != nil {
			ctx = layouts.WithViewer(ctx, layouts.Viewer{Email: who.user.Email, Role: who.role, CSRF: csrf})
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// denySignedOut answers a request without a session: pages go to sign-in,
// HTMX requests are told to load the sign-in page, and
// ?break-glass=1 asks the browser for the shared break-glass login.
func denySignedOut(w http.ResponseWriter, r *http.Request, breakGlass bool) {
	switch {
	case breakGlass && r.URL.Path == "/" && r.URL.Query().Get("break-glass") == "1":
		w.Header().Set("WWW-Authenticate", `Basic realm="overload"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	case r.Header.Get("HX-Request") == "true":
		w.Header().Set("HX-Redirect", "/auth/login")
		http.Error(w, "Sign in again", http.StatusUnauthorized)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	default:
		http.Error(w, "Sign in again", http.StatusUnauthorized)
	}
}

func ValidateAuth() error {
	access, err := accessFromEnv()
	if err != nil {
		return err
	}
	settings, err := oidcFromEnv()
	if err != nil {
		return err
	}
	if access != nil && settings != nil {
		return errors.New("use either Cloudflare Access or OIDC sign-in, not both")
	}
	if access != nil || settings != nil {
		return nil
	}
	if os.Getenv("OVERLOAD_UI_INSECURE") != "1" && (os.Getenv("OVERLOAD_UI_USER") == "" || os.Getenv("OVERLOAD_UI_PASSWORD") == "") {
		return errors.New("OVERLOAD_UI_USER and OVERLOAD_UI_PASSWORD required unless OIDC sign-in (OVERLOAD_OIDC_ISSUER), Cloudflare Access (OVERLOAD_ACCESS_TEAM_DOMAIN, OVERLOAD_ACCESS_AUD) or OVERLOAD_UI_INSECURE=1 is set")
	}
	return nil
}
