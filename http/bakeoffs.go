package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/templates/pages"
	"github.com/jackc/pgx/v5"
)

// BakeoffStore runs and reports model bake-offs.
type BakeoffStore interface {
	ListReviewSettings(context.Context) ([]overload.ReviewSettings, error)
	ListSuites(context.Context) ([]postgres.SuiteSummary, error)
	ListBakeoffs(context.Context) ([]postgres.Bakeoff, error)
	GetBakeoff(context.Context, int64) (postgres.Bakeoff, error)
	QueueBakeoff(ctx context.Context, models, suites []string, runs int) (int64, error)
}

func registerBakeoffs(mux *http.ServeMux, store BakeoffStore, csrf string) {
	mux.HandleFunc("GET /bakeoffs", func(w http.ResponseWriter, r *http.Request) {
		bakeoffs, err := store.ListBakeoffs(r.Context())
		if err != nil {
			http.Error(w, "Unable to load bake-offs", http.StatusInternalServerError)
			return
		}
		models, err := store.ListReviewSettings(r.Context())
		if err != nil {
			http.Error(w, "Unable to load models", http.StatusInternalServerError)
			return
		}
		suites, err := store.ListSuites(r.Context())
		if err != nil {
			http.Error(w, "Unable to load suites", http.StatusInternalServerError)
			return
		}
		_ = pages.Bakeoffs(bakeoffs, models, suites, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /bakeoffs", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		runs, err := strconv.Atoi(strings.TrimSpace(r.PostForm.Get("runs")))
		if err != nil {
			http.Error(w, "Bake-off not started: enter the number of runs.", http.StatusUnprocessableEntity)
			return
		}
		id, err := store.QueueBakeoff(r.Context(), r.PostForm["models"], r.PostForm["suites"], runs)
		if err != nil {
			message := strings.TrimPrefix(err.Error(), postgres.ErrModelPolicy.Error()+": ")
			if errors.Is(err, postgres.ErrModelPolicy) {
				message = "the model policy refuses " + message
			}
			http.Error(w, "Bake-off not started: "+message+".", http.StatusUnprocessableEntity)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/bakeoffs/%d", id), http.StatusSeeOther)
	})
	mux.HandleFunc("GET /bakeoffs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.NotFound(w, r)
			return
		}
		bakeoff, err := store.GetBakeoff(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "Unable to load the bake-off", http.StatusInternalServerError)
			return
		}
		_ = pages.BakeoffDetail(bakeoff, pages.Summarize(bakeoff)).Render(r.Context(), w)
	})
}
