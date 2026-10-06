package httpapi

import (
	"net/http"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/web/templates/pages"
)

func registerAutomation(mux *http.ServeMux, store ConfigurationStore, csrf string) {
	registerWorkflowPreview(mux, store)
	mux.HandleFunc("GET /configure/workflow-agents", func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")
		if kind != "pr_review" && kind != "scheduled_prompt" {
			http.Error(w, "Invalid workflow type", http.StatusBadRequest)
			return
		}
		agents, err := store.ListAgents(r.Context())
		if err != nil {
			http.Error(w, "Unable to load agents", http.StatusInternalServerError)
			return
		}
		selected, _ := workflowFromForm(r.URL.Query())
		selected.Kind = kind
		for index, name := range selected.Agents {
			compatible := ""
			for _, agent := range agents {
				if agent.Name == name && agent.Kind == kind {
					compatible = name
				}
			}
			selected.Agents[index] = compatible
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = pages.WorkflowAgents(selected, agents).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /configure/prompts/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/configure/agents", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /configure/prompts", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/configure/agents", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /configure/workflows", func(w http.ResponseWriter, r *http.Request) {
		workflows, err := store.ListWorkflows(r.Context())
		if err != nil {
			http.Error(w, "Unable to load workflows", http.StatusInternalServerError)
			return
		}
		_ = pages.Workflows(workflows).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /configure/workflows/{name}", func(w http.ResponseWriter, r *http.Request) {
		workflows, err := store.ListWorkflows(r.Context())
		if err != nil {
			http.Error(w, "Unable to load workflows", http.StatusInternalServerError)
			return
		}
		agents, err := store.ListAgents(r.Context())
		if err != nil {
			http.Error(w, "Unable to load agents", http.StatusInternalServerError)
			return
		}
		var selected overload.Workflow
		if r.PathValue("name") == "new" {
			selected.Enabled = true
		} else {
			found := false
			for _, workflow := range workflows {
				if workflow.Name == r.PathValue("name") {
					selected, found = workflow, true
					break
				}
			}
			if !found {
				http.NotFound(w, r)
				return
			}
		}
		if kind := r.URL.Query().Get("kind"); kind == "pr_review" || kind == "scheduled_prompt" {
			selected.Kind = kind
			selected.Agents = nil
		}
		_ = pages.WorkflowForm(selected, agents, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /configure/workflows/{name}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		if err := store.DeleteWorkflow(r.Context(), r.PathValue("name")); err != nil {
			http.Error(w, "Workflow is in use or unavailable", http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/configure/workflows", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /configure/schedules", func(w http.ResponseWriter, r *http.Request) {
		schedules, err := store.ListSchedules(r.Context())
		if err != nil {
			http.Error(w, "Unable to load schedules", http.StatusInternalServerError)
			return
		}
		_ = pages.Schedules(schedules).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /configure/schedules/{name}", func(w http.ResponseWriter, r *http.Request) {
		schedules, err := store.ListSchedules(r.Context())
		if err != nil {
			http.Error(w, "Unable to load schedules", http.StatusInternalServerError)
			return
		}
		workflows, err := store.ListWorkflows(r.Context())
		if err != nil {
			http.Error(w, "Unable to load workflows", http.StatusInternalServerError)
			return
		}
		selected := overload.Schedule{Timezone: "UTC"}
		if r.PathValue("name") != "new" {
			found := false
			for _, schedule := range schedules {
				if schedule.Name == r.PathValue("name") {
					selected, found = schedule, true
					break
				}
			}
			if !found {
				http.NotFound(w, r)
				return
			}
		}
		_ = pages.ScheduleForm(selected, workflows, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /configure/schedules/{name}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		if err := store.DeleteSchedule(r.Context(), r.PathValue("name")); err != nil {
			http.Error(w, "Schedule has run history or is unavailable", http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/configure/schedules", http.StatusSeeOther)
	})
}

func namedResourceMatches(r *http.Request, names []string) bool {
	if r.PostForm.Get("existing") == "" {
		return true
	}
	for _, name := range names {
		if name == r.PostForm.Get("existing") {
			return name == r.PostForm.Get("name")
		}
	}
	return false
}
