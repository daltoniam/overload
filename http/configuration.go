package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/web/templates/pages"
)

type ConfigurationStore interface {
	ListReviewSettings(context.Context) ([]overload.ReviewSettings, error)
	ListPrompts(context.Context) ([]overload.PromptTemplate, error)
	GetPrompt(context.Context, string, string, int) (overload.PromptTemplate, error)
	SavePrompt(context.Context, overload.PromptTemplate) (overload.PromptTemplate, error)
	DeletePrompt(context.Context, string, string) error
	ListAgents(context.Context) ([]overload.AgentDefinition, error)
	SaveAgent(context.Context, overload.AgentDefinition) error
	DeleteAgent(context.Context, string) error
	ListWorkflows(context.Context) ([]overload.Workflow, error)
	ResolveWorkflow(context.Context, string) (overload.ResolvedWorkflow, error)
	SaveWorkflow(context.Context, overload.Workflow) error
	DeleteWorkflow(context.Context, string) error
	ListBindings(context.Context) ([]overload.TriggerBinding, error)
	SaveBinding(context.Context, overload.TriggerBinding) error
	DeleteBinding(context.Context, int64, string) error
	ListRepositories(context.Context) ([]overload.Repository, error)
	SaveRepository(context.Context, string, bool, bool) error
	DeleteRepository(context.Context, int64) error
	ListSchedules(context.Context) ([]overload.Schedule, error)
	SaveSchedule(context.Context, overload.Schedule) error
	DeleteSchedule(context.Context, string) error
}

func registerConfiguration(mux *http.ServeMux, store ConfigurationStore, csrf string) {
	registerAutomation(mux, store, csrf)
	mux.HandleFunc("GET /configure/agents", func(w http.ResponseWriter, r *http.Request) {
		agents, err := store.ListAgents(r.Context())
		if err != nil {
			http.Error(w, "Unable to load agents", http.StatusInternalServerError)
			return
		}
		_ = pages.Agents(agents).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /configure/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		models, err := store.ListReviewSettings(r.Context())
		if err != nil {
			http.Error(w, "Unable to load models", http.StatusInternalServerError)
			return
		}
		prompts, err := store.ListPrompts(r.Context())
		if err != nil {
			http.Error(w, "Unable to load prompts", http.StatusInternalServerError)
			return
		}
		var selected overload.AgentDefinition
		if r.PathValue("name") != "new" {
			agents, err := store.ListAgents(r.Context())
			if err != nil {
				http.Error(w, "Unable to load agents", http.StatusInternalServerError)
				return
			}
			found := false
			for _, agent := range agents {
				if agent.Name == r.PathValue("name") {
					selected, found = agent, true
					break
				}
			}
			if !found {
				http.NotFound(w, r)
				return
			}
		} else {
			selected.Enabled = true
		}
		_ = pages.AgentForm(selected, models, prompts, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /configure/repositories", func(w http.ResponseWriter, r *http.Request) {
		repos, err := store.ListRepositories(r.Context())
		if err != nil {
			http.Error(w, "Unable to load repositories", http.StatusInternalServerError)
			return
		}
		_ = pages.Repositories(repos).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /configure/repositories/{id}", func(w http.ResponseWriter, r *http.Request) {
		repos, err := store.ListRepositories(r.Context())
		if err != nil {
			http.Error(w, "Unable to load repositories", http.StatusInternalServerError)
			return
		}
		bindings, err := store.ListBindings(r.Context())
		if err != nil {
			http.Error(w, "Unable to load bindings", http.StatusInternalServerError)
			return
		}
		workflows, err := store.ListWorkflows(r.Context())
		if err != nil {
			http.Error(w, "Unable to load workflows", http.StatusInternalServerError)
			return
		}
		var selected overload.Repository
		if r.PathValue("id") != "new" {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || id < 1 {
				http.NotFound(w, r)
				return
			}
			found := false
			for _, repo := range repos {
				if repo.ID == id {
					selected, found = repo, true
					break
				}
			}
			if !found {
				http.NotFound(w, r)
				return
			}
		}
		_ = pages.RepositoryForm(selected, bindings, workflows, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /configure/agents/{name}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		if err := store.DeleteAgent(r.Context(), r.PathValue("name")); err != nil {
			http.Error(w, "Agent is in use or unavailable", http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/configure/agents", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /configure/repositories/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.NotFound(w, r)
			return
		}
		if err := store.DeleteRepository(r.Context(), id); err != nil {
			http.Error(w, "Repository has bindings or runs; disable it instead", http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/configure/repositories", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /configure/repositories/{id}/bindings/{binding}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		bindingID, bindingErr := strconv.ParseInt(r.PathValue("binding"), 10, 64)
		if err != nil || bindingErr != nil || id < 1 || bindingID < 1 {
			http.NotFound(w, r)
			return
		}
		repos, err := store.ListRepositories(r.Context())
		if err != nil {
			http.Error(w, "Unable to load repository", http.StatusInternalServerError)
			return
		}
		for _, repo := range repos {
			if repo.ID == id {
				if err := store.DeleteBinding(r.Context(), bindingID, repo.FullName); err != nil {
					http.Error(w, "Binding is in use or unavailable", http.StatusConflict)
					return
				}
				http.Redirect(w, r, "/configure/repositories/"+r.PathValue("id"), http.StatusSeeOther)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /configure", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /configure/{resource}", func(w http.ResponseWriter, r *http.Request) {
		resource := r.PathValue("resource")
		if !validForm(w, r, csrf) {
			return
		}
		var err error
		switch resource {
		case "prompts":
			prompts, listErr := store.ListPrompts(r.Context())
			if listErr != nil {
				http.Error(w, "Unable to load prompts", http.StatusInternalServerError)
				return
			}
			if !promptIdentityMatches(r, prompts) {
				http.Error(w, "Prompt identity cannot be changed", http.StatusBadRequest)
				return
			}
			_, err = store.SavePrompt(r.Context(), overload.PromptTemplate{Name: r.PostForm.Get("name"), Kind: r.PostForm.Get("kind"), Body: r.PostForm.Get("body")})
		case "agents":
			err = store.SaveAgent(r.Context(), overload.AgentDefinition{Name: r.PostForm.Get("name"), Kind: r.PostForm.Get("kind"), Model: r.PostForm.Get("model"), EntryPrompt: r.PostForm.Get("entry_prompt"), Enabled: r.PostForm.Get("enabled") == "true"})
		case "workflows":
			workflows, listErr := store.ListWorkflows(r.Context())
			if listErr != nil {
				http.Error(w, "Unable to load workflows", http.StatusInternalServerError)
				return
			}
			var existing []string
			for _, workflow := range workflows {
				existing = append(existing, workflow.Name)
			}
			if !namedResourceMatches(r, existing) {
				http.Error(w, "Workflow name cannot be changed", http.StatusBadRequest)
				return
			}
			workflow, formErr := workflowFromForm(r.PostForm)
			if formErr == nil {
				formErr = workflow.Validate()
			}
			if formErr != nil {
				http.Error(w, "Workflow not saved: "+formErr.Error(), http.StatusBadRequest)
				return
			}
			if err := store.SaveWorkflow(r.Context(), workflow); err != nil {
				http.Error(w, "Workflow not saved: an agent or prompt is missing, disabled or of the wrong type.", http.StatusBadRequest)
				return
			}
		case "bindings":
			repository := r.PostForm.Get("repository")
			if repository != "" {
				repos, listErr := store.ListRepositories(r.Context())
				if listErr != nil {
					http.Error(w, "Unable to load repository", http.StatusInternalServerError)
					return
				}
				found := false
				for _, repo := range repos {
					if repo.FullName == repository {
						found = true
						break
					}
				}
				if !found {
					http.Error(w, "Repository must be saved first", http.StatusBadRequest)
					return
				}
			}
			err = store.SaveBinding(r.Context(), overload.TriggerBinding{Source: "github", Event: "pull_request", Action: r.PostForm.Get("action"), Repository: repository, Workflow: r.PostForm.Get("workflow"), Enabled: r.PostForm.Get("enabled") == "true"})
		case "repositories":
			if r.PostForm.Get("id") != "" {
				id, parseErr := strconv.ParseInt(r.PostForm.Get("id"), 10, 64)
				repos, listErr := store.ListRepositories(r.Context())
				if parseErr != nil || listErr != nil || id < 1 {
					http.Error(w, "Invalid repository", http.StatusBadRequest)
					return
				}
				found := false
				for _, repo := range repos {
					if repo.ID == id && repo.FullName == r.PostForm.Get("name") {
						found = true
						break
					}
				}
				if !found {
					http.Error(w, "Repository name cannot be changed", http.StatusBadRequest)
					return
				}
			}
			err = store.SaveRepository(r.Context(), r.PostForm.Get("name"), r.PostForm.Get("enabled") == "true", r.PostForm.Get("post") != "true")
		case "schedules":
			schedules, listErr := store.ListSchedules(r.Context())
			if listErr != nil {
				http.Error(w, "Unable to load schedules", http.StatusInternalServerError)
				return
			}
			var existing []string
			for _, schedule := range schedules {
				existing = append(existing, schedule.Name)
			}
			if !namedResourceMatches(r, existing) {
				http.Error(w, "Schedule name cannot be changed", http.StatusBadRequest)
				return
			}
			err = store.SaveSchedule(r.Context(), overload.Schedule{Name: r.PostForm.Get("name"), Workflow: r.PostForm.Get("workflow"), Cron: r.PostForm.Get("cron"), Timezone: r.PostForm.Get("timezone"), Input: json.RawMessage(r.PostForm.Get("input")), Enabled: r.PostForm.Get("enabled") == "true"})
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "Invalid configuration", http.StatusBadRequest)
			return
		}
		destination := "/configure"
		switch resource {
		case "prompts":
			destination = "/configure/prompts"
		case "workflows":
			destination = "/configure/workflows"
		case "schedules":
			destination = "/configure/schedules"
		case "agents":
			destination = "/configure/agents"
		case "repositories":
			destination = "/configure/repositories"
		case "bindings":
			destination = "/configure/repositories"
		}
		if resource == "bindings" || resource == "repositories" {
			repos, listErr := store.ListRepositories(r.Context())
			if listErr == nil {
				name := r.PostForm.Get("repository")
				if resource == "repositories" {
					name = r.PostForm.Get("name")
				}
				for _, repo := range repos {
					if repo.FullName == name {
						destination = "/configure/repositories/" + strconv.FormatInt(repo.ID, 10)
					}
				}
			}
		}
		http.Redirect(w, r, destination, http.StatusSeeOther)
	})
}
