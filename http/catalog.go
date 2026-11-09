package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/web/templates/pages"
)

// PolicyStore reads and saves the model policy.
type PolicyStore interface {
	ListReviewSettings(context.Context) ([]overload.ReviewSettings, error)
	LoadModelPolicy(context.Context) (catalog.Policy, error)
	SaveModelPolicy(context.Context, catalog.Policy) error
}

func registerCatalog(mux *http.ServeMux, store PolicyStore, csrf string) {
	mux.HandleFunc("GET /catalog", func(w http.ResponseWriter, r *http.Request) {
		policy, err := store.LoadModelPolicy(r.Context())
		if err != nil {
			http.Error(w, "Unable to load the model policy", http.StatusInternalServerError)
			return
		}
		_ = pages.Catalog(catalog.Models(), policy).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /catalog/policy", func(w http.ResponseWriter, r *http.Request) {
		policy, err := store.LoadModelPolicy(r.Context())
		if err != nil {
			http.Error(w, "Unable to load the model policy", http.StatusInternalServerError)
			return
		}
		refused, err := refusedModels(r.Context(), store, policy)
		if err != nil {
			http.Error(w, "Unable to load models", http.StatusInternalServerError)
			return
		}
		_ = pages.PolicyForm(policy, catalog.Models(), refused, csrf).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /catalog/{id}", func(w http.ResponseWriter, r *http.Request) {
		model, ok := catalog.Lookup(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		policy, err := store.LoadModelPolicy(r.Context())
		if err != nil {
			http.Error(w, "Unable to load the model policy", http.StatusInternalServerError)
			return
		}
		_ = pages.CatalogEntry(model, policy.Evaluate(model.ID)).Render(r.Context(), w)
	})
	mux.HandleFunc("POST /catalog/policy", func(w http.ResponseWriter, r *http.Request) {
		if !validForm(w, r, csrf) {
			return
		}
		policy := catalog.Policy{
			AllowedOrigins:       splitOrigins(r.PostForm.Get("allowed_origins")),
			BlockedOrigins:       splitOrigins(r.PostForm.Get("blocked_origins")),
			CheckLineage:         r.PostForm.Get("check_lineage") == "true",
			RequireCommercialUse: r.PostForm.Get("require_commercial_use") == "true",
			LicenceReviewed:      r.PostForm["licence_reviewed"],
			LineageReviewed:      r.PostForm["lineage_reviewed"],
			RefuseUncataloged:    r.PostForm.Get("refuse_uncataloged") == "true",
		}
		if value := strings.TrimSpace(r.PostForm.Get("annual_revenue_usd")); value != "" {
			revenue, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				http.Error(w, "Policy not saved: enter the annual revenue as whole dollars.", http.StatusUnprocessableEntity)
				return
			}
			policy.AnnualRevenueUSD = revenue
		}
		policy = policy.Normalize()
		if err := policy.Validate(); err != nil {
			http.Error(w, "Policy not saved: "+err.Error()+".", http.StatusUnprocessableEntity)
			return
		}
		if err := store.SaveModelPolicy(r.Context(), policy); err != nil {
			http.Error(w, "Unable to save the model policy", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/catalog/policy", http.StatusSeeOther)
	})
}

func splitOrigins(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' })
}

// refusedModels describes the saved model connections the policy refuses.
func refusedModels(ctx context.Context, store PolicyStore, policy catalog.Policy) ([]string, error) {
	settings, err := store.ListReviewSettings(ctx)
	if err != nil {
		return nil, err
	}
	var refused []string
	for _, setting := range settings {
		if err := policy.Check(setting.Catalog, setting.Model).Error(); err != nil {
			refused = append(refused, fmt.Sprintf("%s: %v", setting.Name, err))
		}
	}
	return refused, nil
}
