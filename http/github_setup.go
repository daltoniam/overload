package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/templates/pages"
)

type GitHubAppStore interface {
	LoadGitHubApp(context.Context) (github.AppCredentials, error)
	SaveGitHubApp(context.Context, github.AppCredentials) error
	GitHubWebhookSecret(context.Context) (string, error)
	ListGitHubInstallations(context.Context) ([]postgres.GitHubInstallation, error)
	ApplyInstallationEvent(context.Context, string, string, []byte, github.InstallationEvent) (bool, error)
}

// githubAPIBase overrides the GitHub API for manifest conversion in tests.
var githubAPIBase = ""

func publicBaseURL(r *http.Request) string {
	if configured := strings.TrimRight(os.Getenv("OVERLOAD_BASE_URL"), "/"); configured != "" {
		return configured
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func registerGitHubSetup(mux *http.ServeMux, store GitHubAppStore, csrf string) {
	states := newOneTimeTokens(15 * time.Minute)
	render := func(w http.ResponseWriter, r *http.Request, message string) {
		data := pages.GitHubSetupData{CSRF: csrf, DefaultName: "overload-" + strings.NewReplacer(":", "-", ".", "-").Replace(strings.Split(r.Host, ":")[0]), Error: message}
		if len(data.DefaultName) > 34 {
			data.DefaultName = data.DefaultName[:34]
		}
		if base := os.Getenv("OVERLOAD_BASE_URL"); strings.HasPrefix(base, "https://") {
			data.DefaultWebhook = strings.TrimRight(base, "/") + "/webhooks/github"
		}
		if os.Getenv("GITHUB_APP_ID") != "" {
			data.Configured, data.FromEnvironment = true, true
		}
		app, err := store.LoadGitHubApp(r.Context())
		switch {
		case err == nil && !data.FromEnvironment:
			data.Configured, data.AppID, data.Slug, data.HTMLURL = true, app.AppID, app.Slug, app.HTMLURL
		case err != nil && !errors.Is(err, postgres.ErrNoGitHubApp):
			http.Error(w, "Unable to load GitHub App", http.StatusInternalServerError)
			return
		}
		if data.Configured {
			installations, err := store.ListGitHubInstallations(r.Context())
			if err != nil {
				http.Error(w, "Unable to load installations", http.StatusInternalServerError)
				return
			}
			data.Installations = installations
		}
		_ = pages.GitHubSetup(data).Render(r.Context(), w)
	}
	configured := func(r *http.Request) (bool, error) {
		if os.Getenv("GITHUB_APP_ID") != "" {
			return true, nil
		}
		_, err := store.LoadGitHubApp(r.Context())
		if errors.Is(err, postgres.ErrNoGitHubApp) {
			return false, nil
		}
		return err == nil, err
	}
	mux.HandleFunc("GET /setup/github", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, "")
	})
	mux.HandleFunc("POST /setup/github", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		if done, err := configured(r); err != nil || done {
			http.Error(w, "GitHub App already configured", http.StatusConflict)
			return
		}
		manifest, err := github.Manifest(r.PostForm.Get("name"), publicBaseURL(r), r.PostForm.Get("webhook_url"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			render(w, r, err.Error())
			return
		}
		action, err := github.ManifestFormAction(r.PostForm.Get("organization"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			render(w, r, err.Error())
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self' "+themePolicy+"; connect-src 'self'; form-action https://github.com; base-uri 'none'; frame-ancestors 'none'")
		_ = pages.GitHubManifestRedirect(action, manifest, states.issue()).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /setup/github/callback", func(w http.ResponseWriter, r *http.Request) {
		if !states.consume(r.URL.Query().Get("state")) {
			http.Error(w, "Invalid or expired setup state; start again from /setup/github", http.StatusForbidden)
			return
		}
		if done, err := configured(r); err != nil || done {
			http.Error(w, "GitHub App already configured", http.StatusConflict)
			return
		}
		credentials, err := github.CompleteManifest(r.Context(), r.URL.Query().Get("code"), githubAPIBase)
		if err != nil {
			slog.Error("github manifest conversion failed", "error", err)
			http.Error(w, "GitHub App creation could not be completed", http.StatusBadGateway)
			return
		}
		if err := store.SaveGitHubApp(r.Context(), credentials); err != nil {
			http.Error(w, "Unable to save GitHub App", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/setup/github", http.StatusSeeOther)
	})
}

func verifyWebhook(r *http.Request, reader RunReader, body []byte) bool {
	signature := r.Header.Get("X-Hub-Signature-256")
	if github.Verify(body, signature, os.Getenv("GITHUB_WEBHOOK_SECRET")) {
		return true
	}
	store, ok := reader.(GitHubAppStore)
	if !ok {
		return false
	}
	secret, err := store.GitHubWebhookSecret(r.Context())
	return err == nil && github.Verify(body, signature, secret)
}
