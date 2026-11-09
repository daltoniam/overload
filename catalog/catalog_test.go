package catalog

import (
	"strings"
	"testing"
)

func TestCatalogLoads(t *testing.T) {
	all := Models()
	if len(all) < 10 {
		t.Fatalf("catalog has %d entries", len(all))
	}
	flint, ok := Lookup("recursive-flint")
	if !ok || flint.Origin != "US" || flint.Runtime == nil || flint.Runtime.MinBuild != 11801 || flint.Licence.Commercial != CommercialYes {
		t.Fatalf("flint = %+v", flint)
	}
	best, ok := flint.BestEval()
	if !ok || best.Score() != "15 of 20" {
		t.Fatalf("best eval = %+v", best)
	}
	if _, ok := Lookup("missing"); ok {
		t.Fatal("found a missing entry")
	}
	all[0].Name = "changed"
	all[0].Runtime.ServedName = "changed"
	all[0].Weights[0].Quant = "changed"
	if first := Models()[0]; first.Name == "changed" || first.Runtime.ServedName == "changed" || first.Weights[0].Quant == "changed" {
		t.Fatal("Models exposes the catalog for modification")
	}
	flint.Runtime.ServerArgs[0] = "changed"
	lfm, _ := Lookup("lfm2.5-24b-a2b")
	*lfm.Settings.Temperature = 2
	again, _ := Lookup("recursive-flint")
	lfmAgain, _ := Lookup("lfm2.5-24b-a2b")
	if again.Runtime.ServerArgs[0] == "changed" || *lfmAgain.Settings.Temperature != 0.1 {
		t.Fatal("Lookup exposes the catalog for modification")
	}
}

func TestParseRejectsInvalidEntries(t *testing.T) {
	for name, data := range map[string]string{
		"unknown field":   `[{"id":"a","name":"A","publisher":"P","kind":"open","tier":"developer","licence":{"commercial":"yes"},"extra":1}]`,
		"bad origin":      `[{"id":"a","name":"A","publisher":"P","origin":"usa","kind":"open","tier":"developer","licence":{"commercial":"yes"}}]`,
		"bad kind":        `[{"id":"a","name":"A","publisher":"P","kind":"closed","tier":"developer","licence":{"commercial":"yes"}}]`,
		"cap without sum": `[{"id":"a","name":"A","publisher":"P","kind":"open","tier":"developer","licence":{"commercial":"revenue_cap"}}]`,
		"duplicate":       `[{"id":"a","name":"A","publisher":"P","kind":"open","tier":"developer","licence":{"commercial":"yes"}},{"id":"a","name":"A","publisher":"P","kind":"open","tier":"developer","licence":{"commercial":"yes"}}]`,
		"bad eval":        `[{"id":"a","name":"A","publisher":"P","kind":"open","tier":"developer","licence":{"commercial":"yes"},"evals":[{"setup":"x","found":6,"possible":5,"runs":1}]}]`,
		"hosted weights":  `[{"id":"a","name":"A","publisher":"P","kind":"hosted","tier":"hosted","licence":{"commercial":"yes"},"weights":[{"quant":"Q8_0","format":"gguf"}]}]`,
		"reserved id":     `[{"id":"policy","name":"A","publisher":"P","kind":"open","tier":"developer","licence":{"commercial":"yes"}}]`,
		"lineage origin":  `[{"id":"a","name":"A","publisher":"P","kind":"open","tier":"developer","licence":{"commercial":"yes"},"base_origins":["China"]}]`,
	} {
		if _, err := parse([]byte(data)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestPolicyEvaluate(t *testing.T) {
	for _, test := range []struct {
		name    string
		policy  Policy
		id      string
		allowed bool
		reason  string
	}{
		{"zero policy allows everything", Policy{}, "qwen3.8-flash-next", true, ""},
		{"zero policy allows unknown entries", Policy{}, "nope", true, ""},
		{"unknown entry refused under a policy", Policy{BlockedOrigins: []string{"CN"}}, "nope", false, "does not exist"},
		{"blocked origin", Policy{BlockedOrigins: []string{"CN"}}, "qwen3.8-flash-next", false, "from CN, which the model policy blocks"},
		{"allowed origins exclude", Policy{AllowedOrigins: []string{"US"}}, "north-mini-code", false, "from CA, which the model policy does not allow"},
		{"allowed origin", Policy{AllowedOrigins: []string{"US", "CA"}}, "north-mini-code", true, ""},
		{"unknown publisher origin refused when allowlisting", Policy{AllowedOrigins: []string{"US"}}, "bonsai-2-27b", false, "not recorded"},
		{"lineage ignored by default", Policy{BlockedOrigins: []string{"CN"}}, "nemotron-3.5-lightning", true, ""},
		{"lineage checked", Policy{BlockedOrigins: []string{"CN"}, CheckLineage: true}, "nemotron-3.5-lightning", false, "its lineage is from CN"},
		{"base model lineage", Policy{AllowedOrigins: []string{"US"}, CheckLineage: true}, "twil-8b", true, ""},
		{"from scratch lineage", Policy{AllowedOrigins: []string{"US"}, CheckLineage: true}, "recursive-flint", true, ""},
		{"unrecorded base model refused", Policy{AllowedOrigins: []string{"US"}, CheckLineage: true}, "gemma-4-31b", false, "base model is not recorded"},
		{"unrecorded base model with block list", Policy{BlockedOrigins: []string{"CN"}, CheckLineage: true}, "gemma-4-31b", true, ""},
		{"reviewed lineage", Policy{AllowedOrigins: []string{"US"}, CheckLineage: true, LineageReviewed: []string{"gemma-4-31b"}}, "gemma-4-31b", true, ""},
		{"non-commercial refused", Policy{RequireCommercialUse: true}, "twil-8b", false, "does not allow commercial use"},
		{"reviewed licence allowed", Policy{RequireCommercialUse: true, LicenceReviewed: []string{"twil-8b"}}, "twil-8b", true, ""},
		{"revenue cap without revenue", Policy{RequireCommercialUse: true}, "lfm2.5-24b-a2b", false, "below $10,000,000"},
		{"revenue cap above", Policy{RequireCommercialUse: true, AnnualRevenueUSD: 20_000_000}, "lfm2.5-24b-a2b", false, "below $10,000,000"},
		{"revenue cap below", Policy{RequireCommercialUse: true, AnnualRevenueUSD: 5_000_000}, "lfm2.5-24b-a2b", true, ""},
		{"unknown licence", Policy{RequireCommercialUse: true}, "gemma-4-31b", false, "not recorded"},
		{"provider terms", Policy{RequireCommercialUse: true}, "claude-opus-5.5", true, ""},
		{"reviewed licence keeps origin rule", Policy{BlockedOrigins: []string{"CN"}, RequireCommercialUse: true, LicenceReviewed: []string{"qwen3.8-flash-next"}}, "qwen3.8-flash-next", false, "blocks"},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := test.policy.Evaluate(test.id)
			if decision.Allowed != test.allowed {
				t.Fatalf("allowed = %v, reasons %v", decision.Allowed, decision.Reasons)
			}
			if test.reason != "" && !strings.Contains(decision.Error().Error(), test.reason) {
				t.Fatalf("reasons %v do not mention %q", decision.Reasons, test.reason)
			}
			if test.allowed && decision.Error() != nil {
				t.Fatal("allowed decision has an error")
			}
		})
	}
}

func TestPolicyCheckConnections(t *testing.T) {
	block := Policy{BlockedOrigins: []string{"CN"}}
	allow := Policy{AllowedOrigins: []string{"US"}}
	for _, test := range []struct {
		name             string
		policy           Policy
		catalogID, model string
		allowed          bool
		reason           string
	}{
		{"zero policy", Policy{}, "nope", "anything", true, ""},
		{"blank catalog inferred from the model ID", block, "", "qwen3.8-flash-next", false, "blocks"},
		{"inferred from the served name", allow, "", "flint", true, ""},
		{"inference ignores case", block, "", "QWEN3.8-FLASH-NEXT", false, "blocks"},
		{"mislabelled connection", block, "recursive-flint", "qwen3.8-flash-next", false, "is catalog entry qwen3.8-flash-next, not recursive-flint"},
		{"alias keeps its entry", block, "recursive-flint", "my-flint-q8", true, ""},
		{"uncataloged with a block list", block, "", "my-model", true, ""},
		{"uncataloged with an allowlist", allow, "", "my-model", false, "names no catalog entry"},
		{"uncataloged with the licence rule", Policy{RequireCommercialUse: true}, "", "my-model", false, "names no catalog entry"},
		{"uncataloged refused", Policy{RefuseUncataloged: true}, "", "my-model", false, "names no catalog entry"},
		{"cataloged with refuse uncataloged", Policy{RefuseUncataloged: true}, "qwen3.8-flash-next", "qwen3.8-flash-next", true, ""},
		{"unknown named entry", block, "nope", "my-model", false, "does not exist"},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := test.policy.Check(test.catalogID, test.model)
			if decision.Allowed != test.allowed {
				t.Fatalf("allowed = %v, reasons %v", decision.Allowed, decision.Reasons)
			}
			if test.reason != "" && !strings.Contains(decision.Error().Error(), test.reason) {
				t.Fatalf("reasons %v do not mention %q", decision.Reasons, test.reason)
			}
		})
	}
}

func TestFormatUSD(t *testing.T) {
	for value, want := range map[int64]string{0: "$0", 999: "$999", 1000: "$1,000", 10_000_000: "$10,000,000"} {
		if got := FormatUSD(value); got != want {
			t.Errorf("FormatUSD(%d) = %q", value, got)
		}
	}
}

func TestPolicyNormalizeAndValidate(t *testing.T) {
	policy := Policy{AllowedOrigins: []string{" us", "CA", "US", ""}, BlockedOrigins: []string{"cn"}, LicenceReviewed: []string{"twil-8b", "twil-8b"}}.Normalize()
	if strings.Join(policy.AllowedOrigins, ",") != "CA,US" || strings.Join(policy.BlockedOrigins, ",") != "CN" || len(policy.LicenceReviewed) != 1 {
		t.Fatalf("normalized = %+v", policy)
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	if !policy.Restrictive() || (Policy{}).Restrictive() {
		t.Fatal("Restrictive is wrong")
	}
	for name, invalid := range map[string]Policy{
		"bad code":               {AllowedOrigins: []string{"USA"}},
		"both":                   {AllowedOrigins: []string{"US"}, BlockedOrigins: []string{"US"}},
		"negative":               {AnnualRevenueUSD: -1},
		"unknown review":         {LicenceReviewed: []string{"nope"}},
		"unknown lineage review": {LineageReviewed: []string{"nope"}},
	} {
		if err := invalid.Normalize().Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
}
