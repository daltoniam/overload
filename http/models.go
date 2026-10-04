package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/web/templates/pages"
)

type modelDeleter interface {
	DeleteReviewSettings(context.Context, string) error
}

func registerModels(mux *http.ServeMux, reader RunReader, csrf string) {
	mux.HandleFunc("GET /settings/{name}", func(w http.ResponseWriter, r *http.Request) {
		selected := overload.ReviewSettings{ConnectionKind: "local", PromptProfile: "context"}
		if r.PathValue("name") != "new" {
			settings, err := reader.ListReviewSettings(r.Context())
			if err != nil {
				http.Error(w, "Unable to load models", http.StatusInternalServerError)
				return
			}
			found := false
			for _, setting := range settings {
				if setting.Name == r.PathValue("name") {
					selected, found = setting, true
					break
				}
			}
			if !found {
				http.NotFound(w, r)
				return
			}
		}
		_ = pages.ModelForm(selected, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /settings", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		name := r.PostForm.Get("name")
		var previous *overload.ReviewSettings
		settings, err := reader.ListReviewSettings(r.Context())
		if err != nil {
			http.Error(w, "Unable to load models", http.StatusInternalServerError)
			return
		}
		for index := range settings {
			if settings[index].Name == name {
				previous = &settings[index]
				break
			}
		}
		if existing := r.PostForm.Get("existing"); existing != "" && (existing != name || previous == nil) {
			http.Error(w, "Model name cannot be changed", http.StatusBadRequest)
			return
		}
		kind := r.PostForm.Get("connection_kind")
		if kind != "local" && kind != "hosted" {
			http.Error(w, "Invalid model connection type", http.StatusBadRequest)
			return
		}
		setting := overload.ReviewSettings{Name: name, Provider: "openaicompat", ConnectionKind: kind, BaseURL: r.PostForm.Get("base_url"), Model: r.PostForm.Get("model"), APIKeyEnv: r.PostForm.Get("api_key_env"), PromptProfile: "context", IsDefault: r.PostForm.Get("is_default") == "true", Concurrency: 1}
		setting.ReasoningParam = r.PostForm.Get("reasoning_param")
		setting.ReasoningEffort = strings.TrimSpace(r.PostForm.Get("reasoning_effort"))
		if setting.ReasoningParam == "" || setting.ReasoningParam == "none" {
			setting.ReasoningEffort = ""
		}
		if value := strings.TrimSpace(r.PostForm.Get("max_output_tokens")); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				http.Error(w, "Invalid max output tokens", http.StatusBadRequest)
				return
			}
			setting.MaxOutputTokens = parsed
		}
		if value := strings.TrimSpace(r.PostForm.Get("concurrency")); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				http.Error(w, "Invalid concurrency", http.StatusBadRequest)
				return
			}
			setting.Concurrency = parsed
		}
		if previous != nil {
			setting.PromptProfile = previous.PromptProfile
			setting.Agents = previous.Agents
		}
		if err := setting.Validate(); err != nil {
			http.Error(w, "Invalid model settings: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := reader.SaveReviewSettings(r.Context(), setting); err != nil {
			http.Error(w, "Unable to save model", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /settings/{name}/delete", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		deleter, ok := reader.(modelDeleter)
		if !ok {
			http.Error(w, "Model deletion unavailable", http.StatusNotImplemented)
			return
		}
		if err := deleter.DeleteReviewSettings(r.Context(), strings.TrimSpace(r.PathValue("name"))); err != nil {
			http.Error(w, "Model is default, in use or unavailable", http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
	})
}
