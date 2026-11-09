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
	"github.com/daltoniam/overload/gateway"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/assets"
	"github.com/daltoniam/overload/web/templates/layouts"
	"github.com/daltoniam/overload/web/templates/pages"
	"github.com/daltoniam/overload/workload"
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

// Option configures Handler.
type Option func(*handlerOptions)

type handlerOptions struct {
	ingest            PRIngest
	workloadTransport http.RoundTripper
}

// WithPRIngest hands pull request webhooks to ingest instead of only
// recording them.
func WithPRIngest(ingest PRIngest) Option {
	return func(options *handlerOptions) { options.ingest = ingest }
}

// WithWorkloadTransport sets how workload token issuers are reached; tests
// point it at a fake issuer.
func WithWorkloadTransport(transport http.RoundTripper) Option {
	return func(options *handlerOptions) { options.workloadTransport = transport }
}

func Handler(reader RunReader, opts ...Option) http.Handler {
	var options handlerOptions
	for _, option := range opts {
		option(&options)
	}
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
			nonce = formTokenFor(r, csrf)
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
			if options.ingest != nil {
				_, err = options.ingest(r.Context(), deliveryID, action, body, parsed)
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
			auth = newOIDCAuth(settings, sessions)
			registerAuth(mux, auth, csrf)
		} else {
			authErr = errors.New("OIDC sign-in needs the database store")
		}
	}
	if keys, ok := reader.(KeyStore); ok {
		registerKeys(mux, keys, csrf)
	}
	if bakeoffs, ok := reader.(BakeoffStore); ok {
		registerBakeoffs(mux, bakeoffs, csrf)
	}
	nodes, hasNodes := reader.(NodeStore)
	if hasNodes {
		registerNodes(mux, nodes, csrf)
	}
	var verifier *workload.Verifier
	if rules, ok := reader.(WorkloadRuleStore); ok {
		verifier = workload.NewVerifier(rules, options.workloadTransport)
		registerWorkloads(mux, rules, verifier, csrf)
	}
	ui := secure(uiMiddleware(mux, csrf), auth, authErr, csrf)
	routes := http.NewServeMux()
	routes.Handle("/", ui)
	if store, ok := reader.(gateway.Store); ok {
		var extra []gateway.Authenticator
		if verifier != nil {
			extra = append(extra, workloadModels(verifier))
		}
		routes.Handle("/v1/", api(gateway.New(store, extra...)))
	}
	if hasNodes {
		keys, _ := reader.(apiKeyAuthenticator)
		routes.Handle("/api/nodes", api(nodesAPI(nodes, keys)))
	}
	if queuer, ok := reader.(ReviewQueuer); ok {
		keys, _ := reader.(apiKeyAuthenticator)
		routes.Handle("/api/reviews", api(reviewAPI(queuer, keys, verifier, newRerunLimiter(rerunsPerHour, time.Hour))))
	}
	return routes
}

// api serves machine endpoints, which authenticate with API keys or
// workload tokens instead of a browser sign-in. They skip the UI's host
// check: they read no cookies, only bearer credentials, so a rebound or
// unexpected host gains nothing, and in-cluster callers use the service
// name.
func api(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// publicPaths answer without signing in: health checks, signed webhooks
// and the sign-in flow itself.
var publicPaths = map[string]bool{"/healthz": true, "/webhooks/github": true, "/auth/login": true, "/auth/callback": true, "/auth/signed-out": true}

// isPublic reports whether a path answers without signing in. The embedded
// stylesheets and scripts are public so the sign-in and signed-out pages
// can load them.
func isPublic(path string) bool {
	return publicPaths[path] || strings.HasPrefix(path, "/assets/")
}

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
		case auth != nil && isPublic(r.URL.Path):
			who = identity{}
		case auth != nil:
			if signedIn, token, ok := auth.sessionUser(r); ok {
				who = identity{user: &signedIn, role: signedIn.Role, formToken: auth.formToken(token)}
			} else if basicAuth(r) {
				who.breakGlass = true
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					slog.Warn("break-glass change", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "user_agent", r.UserAgent())
				}
			} else {
				auth.clearStaleSession(w, r)
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
		switch {
		case who.user != nil:
			ctx = layouts.WithViewer(ctx, layouts.Viewer{Name: displayName(*who.user), Role: who.role, CSRF: who.formToken})
		case who.breakGlass:
			ctx = layouts.WithViewer(ctx, layouts.Viewer{Role: who.role, BreakGlass: true})
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// denySignedOut answers a request without a session: page loads go to
// sign-in, HTMX requests are told to load the sign-in page, and
// ?break-glass=1 asks the browser for the shared break-glass login. Other
// requests (event streams, scripts, form posts) get a 401 rather than
// starting a sign-in each.
func denySignedOut(w http.ResponseWriter, r *http.Request, breakGlass bool) {
	switch {
	case breakGlass && r.URL.Path == "/" && r.URL.Query().Get("break-glass") == "1":
		w.Header().Set("WWW-Authenticate", `Basic realm="overload"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	case r.Header.Get("HX-Request") == "true":
		w.Header().Set("HX-Redirect", "/auth/login")
		http.Error(w, "Sign in again", http.StatusUnauthorized)
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && isNavigation(r):
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	default:
		signInProblem(w, r, http.StatusUnauthorized, "Your session ended", "Sign in again to continue; anything you were submitting was not saved.")
	}
}

// isNavigation reports whether a request loads a page. Browsers say so with
// Sec-Fetch-Mode; without it, event streams are the only non-page GETs left
// once assets are public.
func isNavigation(r *http.Request) bool {
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return !strings.Contains(r.Header.Get("Accept"), "text/event-stream")
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
