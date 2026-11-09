package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/forge"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/templates/pages"
)

// ForgeStore keeps GitLab and Forgejo connections.
type ForgeStore interface {
	ListForges(context.Context) ([]overload.ForgeConnection, error)
	SaveForge(context.Context, overload.ForgeConnection) (overload.ForgeConnection, error)
	LoadForge(context.Context, int64) (overload.ForgeConnection, error)
	LoadForgeByName(context.Context, string) (overload.ForgeConnection, error)
	DeleteForge(context.Context, int64) error
	ListRepositories(context.Context) ([]overload.Repository, error)
}

// ForgeIngester records a verified GitLab or Forgejo pull request webhook
// and queues a review when a repository and binding match.
type ForgeIngester interface {
	IngestForgePR(ctx context.Context, connection overload.ForgeConnection, deliveryID string, payload []byte, event forge.Event) (bool, error)
}

// newForgeClient builds host clients; tests replace it.
var newForgeClient = forge.New

// ForgeWebhookPath is where a connection's webhooks arrive.
func ForgeWebhookPath(connection overload.ForgeConnection) string {
	return "/webhooks/" + connection.Kind + "/" + strconv.FormatInt(connection.ID, 10)
}

// webhookBase is the public https address git hosts send webhooks to:
// OVERLOAD_BASE_URL, else the Cloudflare Tunnel's hostname, else the
// address the page was opened at.
func webhookBase(r *http.Request, store any) string {
	base := publicBaseURL(r)
	if strings.HasPrefix(base, "https://") {
		return base
	}
	if provider, ok := store.(TunnelProvider); ok && provider.Tunnel() != nil {
		if tunnelBase := provider.Tunnel().Status().Config.BaseURL(); tunnelBase != "" {
			return tunnelBase
		}
	}
	return base
}

func isForgeWebhook(path string) bool {
	return strings.HasPrefix(path, "/webhooks/gitlab/") || strings.HasPrefix(path, "/webhooks/forgejo/")
}

// createForgeWebhook adds the pull request webhook to a repository on a
// connection, using the connection's token.
func createForgeWebhook(ctx context.Context, store ForgeStore, forgeName, repository, baseURL string) error {
	connection, err := store.LoadForgeByName(ctx, forgeName)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(baseURL, "https://") {
		return errors.New("set OVERLOAD_BASE_URL to overload's public https address first")
	}
	client, err := newForgeClient(connection)
	if err != nil {
		return err
	}
	return client.CreateWebhook(ctx, repository, baseURL+ForgeWebhookPath(connection), connection.WebhookSecret)
}

func registerForges(mux *http.ServeMux, store ForgeStore, ingester ForgeIngester, csrf string) {
	render := func(w http.ResponseWriter, r *http.Request, status int, data pages.ForgeSetupData) {
		connections, err := store.ListForges(r.Context())
		if err != nil {
			http.Error(w, "Unable to load git hosts", http.StatusInternalServerError)
			return
		}
		repos, err := store.ListRepositories(r.Context())
		if err != nil {
			http.Error(w, "Unable to load repositories", http.StatusInternalServerError)
			return
		}
		base := webhookBase(r, store)
		data.CSRF, data.PublicBase = csrf, strings.HasPrefix(base, "https://")
		for _, connection := range connections {
			row := pages.ForgeRow{ID: connection.ID, Name: connection.Name, Kind: connection.Kind, BaseURL: connection.BaseURL, WebhookURL: base + ForgeWebhookPath(connection)}
			for _, repo := range repos {
				if repo.ForgeID == connection.ID {
					row.Repositories = append(row.Repositories, repo)
				}
			}
			data.Connections = append(data.Connections, row)
		}
		w.WriteHeader(status)
		_ = pages.ForgeSetup(data).Render(r.Context(), w)
	}
	mux.HandleFunc("GET /setup/forges", func(w http.ResponseWriter, r *http.Request) {
		data := pages.ForgeSetupData{}
		if repo := r.URL.Query().Get("webhook"); repo != "" && len(repo) <= 255 {
			data.Notice = "Webhook created for " + repo + "."
		}
		render(w, r, http.StatusOK, data)
	})
	mux.HandleFunc("POST /setup/forges", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		connection := overload.ForgeConnection{Name: strings.TrimSpace(r.PostForm.Get("name")), Kind: r.PostForm.Get("kind"), BaseURL: r.PostForm.Get("base_url"), Token: strings.TrimSpace(r.PostForm.Get("token"))}
		form := pages.ForgeSetupData{Name: connection.Name, Kind: connection.Kind, BaseURL: connection.BaseURL}
		check := connection
		if check.Token == "" {
			if existing, err := store.LoadForgeByName(r.Context(), connection.Name); err == nil {
				if check.Token, err = overload.ReusableToken(existing, check); err != nil {
					form.Error = err.Error()
					render(w, r, http.StatusUnprocessableEntity, form)
					return
				}
			}
		}
		if err := check.Validate(); err != nil {
			form.Error = err.Error()
			render(w, r, http.StatusUnprocessableEntity, form)
			return
		}
		client, err := newForgeClient(check)
		if err == nil {
			_, err = client.Identity(r.Context())
		}
		if err != nil {
			form.Error = "The token was not accepted by " + check.BaseURL + ": " + overload.TruncateUTF8(err.Error(), 200)
			render(w, r, http.StatusUnprocessableEntity, form)
			return
		}
		if _, err := store.SaveForge(r.Context(), connection); err != nil {
			form.Error = err.Error()
			render(w, r, http.StatusUnprocessableEntity, form)
			return
		}
		http.Redirect(w, r, "/setup/forges", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /setup/forges/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.NotFound(w, r)
			return
		}
		if err := store.DeleteForge(r.Context(), id); err != nil {
			if errors.Is(err, postgres.ErrUnknownForge) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/setup/forges", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /configure/repositories/{id}/webhook", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.NotFound(w, r)
			return
		}
		repos, err := store.ListRepositories(r.Context())
		if err != nil {
			http.Error(w, "Unable to load repositories", http.StatusInternalServerError)
			return
		}
		for _, repo := range repos {
			if repo.ID != id {
				continue
			}
			if repo.Forge == "" {
				http.Error(w, "GitHub repositories get webhooks from the GitHub App", http.StatusBadRequest)
				return
			}
			if err := createForgeWebhook(r.Context(), store, repo.Forge, repo.FullName, webhookBase(r, store)); err != nil {
				http.Error(w, "Webhook not created: "+overload.TruncateUTF8(err.Error(), 300), http.StatusBadGateway)
				return
			}
			http.Redirect(w, r, "/setup/forges?webhook="+url.QueryEscape(repo.FullName), http.StatusSeeOther)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("POST /webhooks/{kind}/{id}", func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("kind")
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if (kind != overload.ForgeGitLab && kind != overload.ForgeForgejo) || err != nil || id < 1 {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		connection, err := store.LoadForge(r.Context(), id)
		if err != nil || connection.Kind != kind || !forge.Verify(kind, r.Header, body, connection.WebhookSecret) {
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}
		deliveryID, event := forge.Delivery(kind, r.Header)
		if deliveryID == "" || len(deliveryID) > 200 {
			http.Error(w, "Missing delivery ID", http.StatusBadRequest)
			return
		}
		parsed, err := forge.Parse(kind, event, body)
		if errors.Is(err, forge.ErrNotPullRequest) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if err != nil {
			http.Error(w, "Invalid pull request event", http.StatusBadRequest)
			return
		}
		if ingester == nil {
			http.Error(w, "Reviews are not available on this server", http.StatusServiceUnavailable)
			return
		}
		if _, err := ingester.IngestForgePR(r.Context(), connection, deliveryID, body, parsed); err != nil {
			http.Error(w, "Unable to ingest delivery", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}
