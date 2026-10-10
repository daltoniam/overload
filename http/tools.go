package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/harness"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/queue"
	"github.com/daltoniam/overload/web/templates/pages"
)

// ToolServerStore manages the MCP servers scheduled agents can call.
type ToolServerStore interface {
	ListToolServers(context.Context) ([]overload.ToolServer, error)
	GetToolServer(context.Context, string) (overload.ToolServer, error)
	SaveToolServer(context.Context, overload.ToolServer) error
	DeleteToolServer(context.Context, string) error
}

// ScheduleRunner queues a schedule's workflow outside its cron times.
type ScheduleRunner interface {
	RunScheduleNow(context.Context, string) (int64, error)
}

// ReviewRequester queues a review of a pull request's current head, as if
// it had just been opened.
type ReviewRequester interface {
	RequestReview(ctx context.Context, repositoryID int64, number int, again bool) (int64, error)
}

// listTools connects to a tool server; a variable so tests can replace it.
var listTools = harness.ListTools

func registerTools(mux *http.ServeMux, store ToolServerStore, csrf string) {
	mux.HandleFunc("GET /configure/tools", func(w http.ResponseWriter, r *http.Request) {
		servers, err := store.ListToolServers(r.Context())
		if err != nil {
			http.Error(w, "Unable to load tool servers", http.StatusInternalServerError)
			return
		}
		_ = pages.ToolServers(servers).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /configure/tools/{name}", func(w http.ResponseWriter, r *http.Request) {
		server := overload.ToolServer{}
		if r.PathValue("name") != "new" {
			found, err := store.GetToolServer(r.Context(), r.PathValue("name"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			server = found
		}
		var check *pages.ToolCheck
		if server.Name != "" && r.URL.Query().Get("check") == "1" {
			check = &pages.ToolCheck{}
			names, err := listTools(r.Context(), server)
			if err != nil {
				check.Error = "Could not connect: " + overload.TruncateUTF8(err.Error(), 500)
			}
			slices.Sort(names)
			check.Tools = names
		}
		_ = pages.ToolServerForm(server, check, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /configure/tools", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		server := overload.ToolServer{Name: strings.TrimSpace(r.PostForm.Get("name")), URL: strings.TrimSpace(r.PostForm.Get("url")), TokenEnv: strings.TrimSpace(r.PostForm.Get("token_env")), Description: strings.TrimSpace(r.PostForm.Get("description")), Enabled: r.PostForm.Get("enabled") == "true"}
		existing := r.PostForm.Get("existing")
		if existing != "" && existing != server.Name {
			http.Error(w, "Tool server name cannot be changed", http.StatusBadRequest)
			return
		}
		if existing == "" {
			if _, err := store.GetToolServer(r.Context(), server.Name); err == nil {
				http.Error(w, "Tool server not saved: one named "+server.Name+" already exists.", http.StatusBadRequest)
				return
			}
		}
		if err := server.Validate(); err != nil {
			http.Error(w, "Tool server not saved: "+err.Error()+".", http.StatusBadRequest)
			return
		}
		if err := store.SaveToolServer(r.Context(), server); err != nil {
			http.Error(w, "Unable to save tool server", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/configure/tools/"+server.Name, http.StatusSeeOther)
	})
	mux.HandleFunc("POST /configure/tools/{name}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		if err := store.DeleteToolServer(r.Context(), r.PathValue("name")); err != nil {
			message := "Tool server is unavailable"
			if errors.Is(err, postgres.ErrToolServerInUse) {
				message = "Tool server not deleted: " + strings.TrimPrefix(err.Error(), postgres.ErrToolServerInUse.Error()+": ") + ". Disable it instead."
			}
			http.Error(w, message, http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/configure/tools", http.StatusSeeOther)
	})
}

func registerReviewRequests(mux *http.ServeMux, requester ReviewRequester, csrf string) {
	mux.HandleFunc("POST /configure/repositories/{id}/review", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		repositoryID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || repositoryID < 1 {
			http.NotFound(w, r)
			return
		}
		number, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(r.PostForm.Get("pr"), "#")))
		if err != nil || number < 1 {
			http.Error(w, "Enter a pull request number", http.StatusBadRequest)
			return
		}
		runID, err := requester.RequestReview(r.Context(), repositoryID, number, r.PostForm.Get("again") == "true")
		if err != nil {
			message := "Review not queued"
			if errors.Is(err, queue.ErrReviewNotQueued) {
				message = "Review not queued: " + strings.TrimPrefix(err.Error(), queue.ErrReviewNotQueued.Error()+": ")
			}
			http.Error(w, message+".", http.StatusConflict)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/runs/%d", runID), http.StatusSeeOther)
	})
}

func registerScheduleRunner(mux *http.ServeMux, runner ScheduleRunner, csrf string) {
	mux.HandleFunc("POST /configure/schedules/{name}/run", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		runID, err := runner.RunScheduleNow(r.Context(), r.PathValue("name"))
		if err != nil {
			message := "Run not queued"
			if errors.Is(err, postgres.ErrWorkflowUnavailable) {
				message += ": " + strings.TrimPrefix(err.Error(), postgres.ErrWorkflowUnavailable.Error()+": ")
			}
			http.Error(w, message, http.StatusConflict)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/runs/%d", runID), http.StatusSeeOther)
	})
}
