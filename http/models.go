package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/templates/pages"
)

type modelDeleter interface {
	DeleteReviewSettings(context.Context, string) error
}

// BudgetStore keeps spending caps.
type BudgetStore interface {
	Budgets(context.Context) ([]postgres.Budget, error)
	SaveBudget(context.Context, postgres.Budget) error
}

func registerModels(mux *http.ServeMux, reader RunReader, csrf string) {
	if store, ok := reader.(BudgetStore); ok {
		mux.HandleFunc("POST /settings/budget", func(w http.ResponseWriter, r *http.Request) {
			if !validForm(w, r, csrf) {
				return
			}
			budget := postgres.Budget{SkipStatus: r.PostForm.Get("skip_status")}
			for _, field := range []struct {
				name   string
				target **float64
			}{{"monthly_usd", &budget.MonthlyUSD}, {"daily_usd", &budget.DailyUSD}} {
				value, err := overload.ParsePrice(r.PostForm.Get(field.name))
				if err != nil {
					http.Error(w, "Budget caps must be dollar amounts", http.StatusBadRequest)
					return
				}
				*field.target = value
			}
			if err := store.SaveBudget(r.Context(), budget); err != nil {
				http.Error(w, "Budget not saved: "+err.Error(), http.StatusBadRequest)
				return
			}
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
		})
	}
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
		provider := r.PostForm.Get("provider")
		if provider == "" {
			provider = overload.ProviderOpenAICompat
		}
		setting := overload.ReviewSettings{Name: name, Provider: provider, ConnectionKind: kind, BaseURL: r.PostForm.Get("base_url"), Model: r.PostForm.Get("model"), APIKeyEnv: r.PostForm.Get("api_key_env"), PromptProfile: "context", IsDefault: r.PostForm.Get("is_default") == "true", Concurrency: 1}
		setting.API = r.PostForm.Get("api")
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
		for _, price := range []struct {
			field  string
			target **float64
		}{{"price_input", &setting.PriceInput}, {"price_cached_input", &setting.PriceCachedInput}, {"price_cache_write", &setting.PriceCacheWrite}, {"price_output", &setting.PriceOutput}} {
			value, err := overload.ParsePrice(r.PostForm.Get(price.field))
			if err != nil {
				http.Error(w, "Invalid model settings: "+err.Error(), http.StatusBadRequest)
				return
			}
			*price.target = value
		}
		if value := strings.TrimSpace(r.PostForm.Get("long_context_tokens")); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				http.Error(w, "Invalid long-context threshold", http.StatusBadRequest)
				return
			}
			setting.LongContextTokens = parsed
		}
		for _, multiplier := range []struct {
			field  string
			target *float64
		}{{"long_context_input_multiplier", &setting.LongContextInputMultiplier}, {"long_context_output_multiplier", &setting.LongContextOutputMultiplier}} {
			value, err := overload.ParsePrice(r.PostForm.Get(multiplier.field))
			if err != nil {
				http.Error(w, "Invalid long-context multiplier", http.StatusBadRequest)
				return
			}
			if value != nil {
				*multiplier.target = *value
			}
		}
		if previous != nil {
			setting.PromptProfile = previous.PromptProfile
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
