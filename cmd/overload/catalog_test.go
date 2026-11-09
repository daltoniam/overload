package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

func TestManageCatalog(t *testing.T) {
	var out bytes.Buffer
	if err := manageCatalog([]string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "recursive-flint") || !strings.Contains(out.String(), "non-commercial") || !strings.Contains(out.String(), "free below $10,000,000 revenue") {
		t.Fatalf("list = %s", out.String())
	}
	out.Reset()
	if err := manageCatalog([]string{"show", "twil-8b"}, &out); err != nil || !strings.Contains(out.String(), `"base_model": "IBM Granite 4.2 8B"`) {
		t.Fatalf("show = %s, %v", out.String(), err)
	}
	if err := manageCatalog([]string{"show", "missing"}, &out); err == nil {
		t.Fatal("showed a missing entry")
	}
	if err := manageCatalog([]string{"list", "extra"}, &out); err == nil {
		t.Fatal("accepted extra arguments")
	}
}

func TestParsePolicyFlags(t *testing.T) {
	policy, err := parsePolicyFlags([]string{"--allow-origins", "us,ca", "--block-origins", "cn", "--check-lineage", "--require-commercial-use", "--licence-reviewed", "twil-8b", "--annual-revenue-usd", "1000"})
	if err != nil || strings.Join(policy.AllowedOrigins, ",") != "CA,US" || !policy.CheckLineage || !policy.RequireCommercialUse || policy.AnnualRevenueUSD != 1000 || policy.LicenceReviewed[0] != "twil-8b" {
		t.Fatalf("policy = %+v, %v", policy, err)
	}
	if policy, err := parsePolicyFlags(nil); err != nil || policy.Restrictive() {
		t.Fatalf("empty flags = %+v, %v", policy, err)
	}
	for _, args := range [][]string{{"--allow-origins", "USA"}, {"--licence-reviewed", "nope"}, {"extra"}} {
		if _, err := parsePolicyFlags(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestApplyCatalogDefaults(t *testing.T) {
	setting := overload.ReviewSettings{Catalog: "qwen3.8-flash-next", Model: "", ConnectionKind: "local"}
	if err := applyCatalogDefaults(&setting, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if setting.Model != "qwen3.8-flash-next" || setting.ReasoningParam != "reasoning_effort" || setting.ReasoningEffort != "high" || setting.MaxOutputTokens != 28000 {
		t.Fatalf("defaults = %+v", setting)
	}
	setting = overload.ReviewSettings{Catalog: "recursive-flint", Model: "my-flint", ReasoningParam: "chat_template", ReasoningEffort: "low"}
	if err := applyCatalogDefaults(&setting, map[string]bool{"model": true, "reasoning-param": true}); err != nil {
		t.Fatal(err)
	}
	if setting.Model != "my-flint" || setting.ReasoningParam != "chat_template" || setting.ReasoningEffort != "low" {
		t.Fatalf("given flags were overridden: %+v", setting)
	}
	setting = overload.ReviewSettings{Catalog: "claude-opus-5.5", ConnectionKind: "local", Provider: overload.ProviderOpenAICompat}
	if err := applyCatalogDefaults(&setting, map[string]bool{}); err != nil || setting.ConnectionKind != "hosted" || setting.Provider != overload.ProviderAnthropic || setting.ReasoningEffort != "high" {
		t.Fatalf("hosted default = %+v, %v", setting, err)
	}
	if err := applyCatalogDefaults(&overload.ReviewSettings{Catalog: "nope"}, nil); err == nil {
		t.Fatal("accepted an unknown entry")
	}
}
