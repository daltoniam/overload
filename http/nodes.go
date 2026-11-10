package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/web/templates/pages"
	"github.com/jackc/pgx/v5"
)

// NodeStore keeps the node inventory and placements.
type NodeStore interface {
	ReportNode(ctx context.Context, keyID int64, report overload.NodeReport) error
	ListNodes(context.Context) ([]overload.Node, error)
	DeleteNode(context.Context, string) error
	ReleaseNode(context.Context, string) error
	ListPlacements(context.Context) ([]overload.Placement, error)
}

// maxNodeReport bounds a report body; a full report is far smaller.
const maxNodeReport = 256 << 10

// nodesAPI accepts a node's report from an API key with the nodes scope.
// Node reports shape cluster placements, so the key must belong to an
// administrator or have been created on the server with the CLI.
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
		key, user, err := keys.AuthenticateAPIKey(r.Context(), token)
		if err != nil || !key.HasScope(overload.ScopeNodes) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key, or it cannot report nodes"})
			return
		}
		if user != nil && !overload.RoleAtLeast(user.Role, overload.RoleAdmin) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only an administrator's key, or one created with overload keys create, can report nodes"})
			return
		}
		var report overload.NodeReport
		// Unknown fields are ignored so newer nodes can report to an older
		// control plane during an upgrade.
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxNodeReport))
		if err := decoder.Decode(&report); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send one JSON report of at most 256 KiB"})
			return
		}
		err = store.ReportNode(r.Context(), key.ID, report)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, overload.ErrNodeClaimed), errors.Is(err, overload.ErrNodeReleased):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		case errors.Is(err, overload.ErrInvalidNodeReport), errors.Is(err, overload.ErrTooManyNodes):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		default:
			slog.Error("node report not stored", "node", report.Name, "key_prefix", key.Prefix, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "report not stored"})
		}
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
		_ = pages.Nodes(nodes, overload.Drift(nodes, now), placements, now, formTokenFor(r, csrf)).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /nodes/{name}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		err := store.DeleteNode(r.Context(), r.PathValue("name"))
		switch {
		case err == nil:
			http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		case errors.Is(err, pgx.ErrNoRows):
			http.NotFound(w, r)
		case errors.Is(err, overload.ErrNodeInPlacement):
			http.Error(w, "Node not forgotten: "+strings.TrimPrefix(err.Error(), overload.ErrNodeInPlacement.Error()+": ")+".", http.StatusConflict)
		default:
			http.Error(w, "Unable to forget the node", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /nodes/{name}/release", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		err := store.ReleaseNode(r.Context(), r.PathValue("name"))
		switch {
		case err == nil:
			http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		case errors.Is(err, pgx.ErrNoRows):
			http.NotFound(w, r)
		default:
			http.Error(w, "Unable to release the node", http.StatusInternalServerError)
		}
	})
}
