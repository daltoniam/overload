package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/web/templates/pages"
	"github.com/jackc/pgx/v5"
)

// identity is who made a request and the role they act with.
type identity struct {
	user *overload.User
	role string
	// breakGlass marks the shared Basic Auth login used while OIDC is
	// configured.
	breakGlass bool
}

type identityKey struct{}

func withIdentity(ctx context.Context, who identity) context.Context {
	return context.WithValue(ctx, identityKey{}, who)
}

func identityFrom(ctx context.Context) identity {
	who, _ := ctx.Value(identityKey{}).(identity)
	return who
}

// memberActions are the state changes members may make; every other change
// needs an admin.
var memberActions = []*regexp.Regexp{
	regexp.MustCompile(`^/configure/repositories/[0-9]+/review$`),
	regexp.MustCompile(`^/configure/schedules/[^/]+/run$`),
	regexp.MustCompile(`^/auth/logout$`),
}

// adminPages are pages only admins may read.
var adminPages = []*regexp.Regexp{regexp.MustCompile(`^/users(/|$)`)}

// allowed reports whether role may make the request.
func allowed(role string, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		for _, page := range adminPages {
			if page.MatchString(r.URL.Path) {
				return overload.RoleAtLeast(role, overload.RoleAdmin)
			}
		}
		return overload.RoleAtLeast(role, overload.RoleViewer)
	}
	if overload.RoleAtLeast(role, overload.RoleAdmin) {
		return true
	}
	for _, action := range memberActions {
		if action.MatchString(r.URL.Path) {
			return r.URL.Path == "/auth/logout" || overload.RoleAtLeast(role, overload.RoleMember)
		}
	}
	return false
}

func registerAuth(mux *http.ServeMux, auth *oidcAuth, csrf string) {
	mux.HandleFunc("GET /auth/login", auth.login)
	mux.HandleFunc("GET /auth/callback", auth.callback)
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		auth.logout(w, r)
	})
	mux.HandleFunc("GET /auth/signed-out", func(w http.ResponseWriter, r *http.Request) {
		_ = pages.SignedOut(auth.endSessionURL(r.Context())).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {
		users, err := auth.store.ListUsers(r.Context())
		if err != nil {
			http.Error(w, "Unable to load users", http.StatusInternalServerError)
			return
		}
		var self int64
		if who := identityFrom(r.Context()); who.user != nil {
			self = who.user.ID
		}
		_ = pages.Users(users, self, csrf).Render(r.Context(), w)
	})
	for action, disabled := range map[string]bool{"disable": true, "enable": false} {
		mux.HandleFunc("POST /users/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			if !validForm(w, r, csrf) {
				return
			}
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || id < 1 {
				http.NotFound(w, r)
				return
			}
			who := identityFrom(r.Context())
			if disabled && who.user != nil && who.user.ID == id {
				http.Error(w, "You cannot disable your own account.", http.StatusConflict)
				return
			}
			if err := auth.store.SetUserDisabled(r.Context(), id, disabled); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					http.NotFound(w, r)
					return
				}
				http.Error(w, "Unable to update the user", http.StatusInternalServerError)
				return
			}
			slog.Info("user access changed", "user_id", id, "disabled", disabled, "by_user_id", actorID(who), "break_glass", who.breakGlass)
			http.Redirect(w, r, "/users", http.StatusSeeOther)
		})
	}
}

func actorID(who identity) int64 {
	if who.user == nil {
		return 0
	}
	return who.user.ID
}
