package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/web/templates/layouts"
	"github.com/daltoniam/overload/web/templates/pages"
)

// KeyStore manages API keys and reports gateway usage.
type KeyStore interface {
	CreateAPIKey(ctx context.Context, userID int64, name string, scopes []string, lifetime time.Duration) (overload.APIKey, string, error)
	ListAPIKeys(ctx context.Context, userID int64, all bool) ([]overload.APIKey, error)
	RevokeAPIKey(ctx context.Context, id, userID int64, anyOwner bool) error
	GetAPIKey(ctx context.Context, id, userID int64, anyOwner bool) (overload.APIKey, error)
	UsageTotals(ctx context.Context, since time.Time, userID int64, all bool) ([]overload.UsageTotal, error)
}

// owner returns the user a request acts for (0 without OIDC sign-in) and
// whether they may see and manage everyone's keys and usage.
func owner(ctx context.Context) (int64, bool) {
	who := identityFrom(ctx)
	return actorID(who), overload.RoleAtLeast(who.role, overload.RoleAdmin)
}

// gatewayBase is the address agents reach overload at: OVERLOAD_BASE_URL,
// or the address overload listens on.
func gatewayBase() string {
	if base := strings.TrimRight(os.Getenv("OVERLOAD_BASE_URL"), "/"); base != "" {
		return base
	}
	addr := os.Getenv("OVERLOAD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8082"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8082"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func keysPage(ctx context.Context, keys []overload.APIKey, all bool, created *pages.NewKey) pages.KeysPage {
	return pages.KeysPage{Keys: keys, All: all, CanCreate: overload.RoleAtLeast(identityFrom(ctx).role, overload.RoleMember), Endpoint: gatewayBase() + "/v1", Created: created, Now: time.Now()}
}

func registerKeys(mux *http.ServeMux, store KeyStore, csrf string) {
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		userID, all := owner(r.Context())
		keys, err := store.ListAPIKeys(r.Context(), userID, all)
		if err != nil {
			http.Error(w, "Unable to load API keys", http.StatusInternalServerError)
			return
		}
		_ = pages.APIKeys(keysPage(r.Context(), keys, all, nil), formTokenFor(r, csrf)).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /keys", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		days, err := strconv.Atoi(strings.TrimSpace(r.PostForm.Get("days")))
		if err != nil {
			http.Error(w, "Key not created: enter how many days it lasts.", http.StatusUnprocessableEntity)
			return
		}
		scopes := r.PostForm["scopes"]
		lifetime := time.Duration(days) * 24 * time.Hour
		name := strings.TrimSpace(r.PostForm.Get("name"))
		if err := overload.ValidateAPIKey(name, scopes, lifetime); err != nil {
			http.Error(w, "Key not created: "+err.Error()+".", http.StatusUnprocessableEntity)
			return
		}
		userID, all := owner(r.Context())
		key, secret, err := store.CreateAPIKey(r.Context(), userID, name, scopes, lifetime)
		if err != nil {
			http.Error(w, "Unable to create the key", http.StatusInternalServerError)
			return
		}
		keys, err := store.ListAPIKeys(r.Context(), userID, all)
		if err != nil {
			// The secret is shown only now, so show it even when the rest
			// of the list cannot be loaded.
			slog.Warn("listing API keys after creating one failed", "error", err)
			keys = []overload.APIKey{key}
		}
		page := keysPage(r.Context(), keys, all, &pages.NewKey{Key: key, Secret: secret})
		page.ListFailed = err != nil
		w.Header().Set("Cache-Control", "no-store")
		_ = pages.APIKeys(page, formTokenFor(r, csrf)).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /keys/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.NotFound(w, r)
			return
		}
		userID, all := owner(r.Context())
		if r.PostForm.Get("confirmed") != "true" {
			confirmRevoke(w, r, store, csrf, id)
			return
		}
		if err := store.RevokeAPIKey(r.Context(), id, userID, all); err != nil {
			if errors.Is(err, overload.ErrInvalidAPIKey) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Unable to revoke the key", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/keys", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /usage", func(w http.ResponseWriter, r *http.Request) {
		userID, all := owner(r.Context())
		since := time.Now().Add(-30 * 24 * time.Hour)
		totals, err := store.UsageTotals(r.Context(), since, userID, all)
		if err != nil {
			http.Error(w, "Unable to load usage", http.StatusInternalServerError)
			return
		}
		_ = pages.Usage(totals, all).Render(r.Context(), w)
	})
}

// confirmRevoke asks before revoking key id, naming it, since agents using
// it stop working at once. Without JavaScript this page is the only check.
func confirmRevoke(w http.ResponseWriter, r *http.Request, store KeyStore, csrf string, id int64) {
	userID, all := owner(r.Context())
	key, err := store.GetAPIKey(r.Context(), id, userID, all)
	if errors.Is(err, overload.ErrInvalidAPIKey) || err == nil && key.State(time.Now()) != overload.APIKeyActive {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Unable to load the API key", http.StatusInternalServerError)
		return
	}
	prompt := pages.ConfirmPrompt{Eyebrow: "API keys / Revoke", Title: "Revoke API key " + key.Name + "?", Impact: pages.RevokeImpact(key), Button: "Revoke key", Cancel: "/keys"}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = layouts.Base(layouts.PageData{Title: "Revoke API key", CurrentPath: "/keys"}).Render(templ.WithChildren(r.Context(), pages.Confirmation(r.URL.Path, formTokenFor(r, csrf), r.PostForm, prompt)), w)
}
