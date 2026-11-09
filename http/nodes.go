package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/web/templates/pages"
	"github.com/jackc/pgx/v5"
)

// NodeStore keeps the node inventory and placements.
type NodeStore interface {
	ReportNode(context.Context, overload.NodeReport) error
	ListNodes(context.Context) ([]overload.Node, error)
	DeleteNode(context.Context, string) error
	ListPlacements(context.Context) ([]overload.Placement, error)
}

// nodesAPI accepts a node's report from an API key with the nodes scope.
func nodesAPI(store NodeStore, keys apiKeyAuthenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
			return
		}
		token := bearerToken(r)
		if keys == nil || !strings.HasPrefix(token, "ovl_") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "send an overload API key with the nodes scope"})
			return
		}
		key, _, err := keys.AuthenticateAPIKey(r.Context(), token)
		if err != nil || !key.HasScope(overload.ScopeNodes) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key, or it cannot report nodes"})
			return
		}
		var report overload.NodeReport
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&report); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid report"})
			return
		}
		if err := store.ReportNode(r.Context(), report); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func registerNodes(mux *http.ServeMux, store NodeStore, csrf string) {
	mux.HandleFunc("GET /nodes", func(w http.ResponseWriter, r *http.Request) {
		nodes, err := store.ListNodes(r.Context())
		if err != nil {
			http.Error(w, "Unable to load nodes", http.StatusInternalServerError)
			return
		}
		placements, err := store.ListPlacements(r.Context())
		if err != nil {
			http.Error(w, "Unable to load placements", http.StatusInternalServerError)
			return
		}
		now := time.Now()
		_ = pages.Nodes(nodes, overload.Drift(nodes, now), placements, now, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /nodes/{name}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		if err := store.DeleteNode(r.Context(), r.PathValue("name")); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Unable to remove the node", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
	})
}
