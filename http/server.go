package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/assets"
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
		_ = pages.Models(settings).Render(r.Context(), w)
	})
	registerModels(mux, reader, csrf)
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
	return secure(uiMiddleware(mux, csrf))
}

func secure(next http.Handler) http.Handler {
	user := os.Getenv("OVERLOAD_UI_USER")
	password := os.Getenv("OVERLOAD_UI_PASSWORD")
	insecure := os.Getenv("OVERLOAD_UI_INSECURE") == "1"
	hosts := newHostPolicy(os.Getenv("OVERLOAD_BASE_URL"))
	access, accessErr := accessFromEnv()
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
		if access != nil {
			if _, err := access.verify(r.Context(), r.Header.Get(accessHeader)); err != nil {
				http.Error(w, "Sign in through Cloudflare Access", http.StatusForbidden)
				return
			}
		} else if !insecure {
			givenUser, givenPassword, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(givenUser), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(givenPassword), []byte(password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="overload"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) {
				http.Error(w, "Invalid origin", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func ValidateAuth() error {
	access, err := accessFromEnv()
	if err != nil {
		return err
	}
	if access != nil {
		return nil
	}
	if os.Getenv("OVERLOAD_UI_INSECURE") != "1" && (os.Getenv("OVERLOAD_UI_USER") == "" || os.Getenv("OVERLOAD_UI_PASSWORD") == "") {
		return errors.New("OVERLOAD_UI_USER and OVERLOAD_UI_PASSWORD required unless Cloudflare Access (OVERLOAD_ACCESS_TEAM_DOMAIN, OVERLOAD_ACCESS_AUD) or OVERLOAD_UI_INSECURE=1 is set")
	}
	return nil
}
