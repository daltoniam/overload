package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
)

func manageCatalog(args []string, out io.Writer) error {
	usage := errors.New("usage: overload catalog list [--json] | show ID")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "list":
		flags := flag.NewFlagSet("catalog list", flag.ContinueOnError)
		asJSON := flags.Bool("json", false, "print JSON")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return usage
		}
		if *asJSON {
			return json.NewEncoder(out).Encode(catalog.Models())
		}
		writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "ID\tPUBLISHER\tORIGIN\tLICENCE\tTIER\tBEST EVAL")
		for _, model := range catalog.Models() {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", model.ID, model.Publisher, originLabel(model.Origin), licenceLabel(model.Licence), model.Tier, bestEvalLabel(model))
		}
		return writer.Flush()
	case "show":
		if len(args) != 2 {
			return usage
		}
		model, ok := catalog.Lookup(args[1])
		if !ok {
			return fmt.Errorf("catalog entry %q does not exist", args[1])
		}
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(model)
	default:
		return usage
	}
}

func originLabel(origin string) string {
	if origin == "" {
		return "not recorded"
	}
	return origin
}

func licenceLabel(licence catalog.Licence) string {
	switch licence.Commercial {
	case catalog.CommercialYes:
		return licence.Name
	case catalog.CommercialNo:
		return "non-commercial"
	case catalog.CommercialRevenueCap:
		return "free below " + catalog.FormatUSD(licence.RevenueCapUSD) + " revenue"
	case catalog.CommercialProvider:
		return "provider terms"
	default:
		return "not recorded"
	}
}

func bestEvalLabel(model catalog.Model) string {
	best, ok := model.BestEval()
	if !ok {
		return "not measured"
	}
	return best.Score()
}

func managePolicy(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	usage := errors.New("usage: overload policy show | set [--allow-origins US,CA] [--block-origins CN] [--check-lineage] [--require-commercial-use] [--annual-revenue-usd N] [--licence-reviewed id,id] [--lineage-reviewed id,id] [--refuse-uncataloged]")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "show":
		if len(args) != 1 {
			return usage
		}
		policy, err := store.LoadModelPolicy(ctx)
		if err != nil {
			return err
		}
		models, err := store.ListReviewSettings(ctx)
		if err != nil {
			return err
		}
		return writePolicyReport(os.Stdout, policy, models)
	case "set":
		policy, err := parsePolicyFlags(args[1:])
		if err != nil {
			return err
		}
		if err := store.SaveModelPolicy(ctx, policy); err != nil {
			return err
		}
		models, err := store.ListReviewSettings(ctx)
		if err != nil {
			return err
		}
		return writePolicyReport(os.Stdout, policy.Normalize(), models)
	default:
		return usage
	}
}

// parsePolicyFlags reads a whole policy; flags not given take their
// permissive defaults, so `policy set` with no flags clears the policy.
func parsePolicyFlags(args []string) (catalog.Policy, error) {
	flags := flag.NewFlagSet("policy set", flag.ContinueOnError)
	allow := flags.String("allow-origins", "", "comma-separated country codes; only these publishers are allowed")
	block := flags.String("block-origins", "", "comma-separated country codes to refuse")
	lineage := flags.Bool("check-lineage", false, "apply origin rules to base models and training-data sources")
	commercial := flags.Bool("require-commercial-use", false, "refuse models whose licence does not allow commercial use or is not recorded")
	revenue := flags.Int64("annual-revenue-usd", 0, "annual revenue, for licences free below a revenue cap")
	reviewed := flags.String("licence-reviewed", "", "comma-separated catalog IDs whose licence you have confirmed")
	lineageReviewed := flags.String("lineage-reviewed", "", "comma-separated catalog IDs whose base model you have confirmed")
	uncataloged := flags.Bool("refuse-uncataloged", false, "refuse model connections that match no catalog entry")
	if err := flags.Parse(args); err != nil {
		return catalog.Policy{}, err
	}
	if flags.NArg() != 0 {
		return catalog.Policy{}, errors.New("unexpected policy arguments")
	}
	policy := catalog.Policy{AllowedOrigins: splitList(*allow), BlockedOrigins: splitList(*block), CheckLineage: *lineage, RequireCommercialUse: *commercial, AnnualRevenueUSD: *revenue, LicenceReviewed: splitList(*reviewed), LineageReviewed: splitList(*lineageReviewed), RefuseUncataloged: *uncataloged}.Normalize()
	return policy, policy.Validate()
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func writePolicyReport(out io.Writer, policy catalog.Policy, models []overload.ReviewSettings) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(policy); err != nil {
		return err
	}
	for _, model := range models {
		if err := policy.Check(model.Catalog, model.Model).Error(); err != nil {
			if _, err := fmt.Fprintf(out, "refused: model %s: %v\n", model.Name, err); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyCatalogDefaults fills settings the user did not pass from the
// catalog entry's known-good values.
func applyCatalogDefaults(setting *overload.ReviewSettings, given map[string]bool) error {
	if setting.Catalog == "" {
		return nil
	}
	model, ok := catalog.Lookup(setting.Catalog)
	if !ok {
		return fmt.Errorf("catalog entry %q does not exist", setting.Catalog)
	}
	if !given["model"] && model.Runtime != nil && model.Runtime.ServedName != "" {
		setting.Model = model.Runtime.ServedName
	}
	if !given["provider"] && model.Settings.Provider != "" {
		setting.Provider = model.Settings.Provider
	}
	// The catalog's reasoning settings were measured through its provider's
	// API; another API takes them differently or ignores them.
	if !given["reasoning-param"] && !given["reasoning-effort"] && cmp.Or(setting.Provider, overload.ProviderOpenAICompat) == cmp.Or(model.Settings.Provider, overload.ProviderOpenAICompat) {
		setting.ReasoningParam, setting.ReasoningEffort = model.Settings.ReasoningParam, model.Settings.ReasoningEffort
	}
	if !given["max-output-tokens"] && model.Settings.MaxOutputTokens != 0 {
		setting.MaxOutputTokens = model.Settings.MaxOutputTokens
	}
	if !given["connection-kind"] && model.Kind == catalog.KindHosted {
		setting.ConnectionKind = "hosted"
	}
	return nil
}
