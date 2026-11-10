package overload

import (
	"math"
	"strings"
	"testing"
)

func price(value float64) *float64 { return &value }

func TestReviewSettingsPrices(t *testing.T) {
	base := ReviewSettings{Name: "sol", Provider: "openaicompat", ConnectionKind: "hosted", BaseURL: "https://api.openai.com/v1", Model: "gpt-6.1-sol", APIKeyEnv: "OVERLOAD_MODEL_KEY", PromptProfile: "context"}
	tests := []struct {
		name    string
		edit    func(*ReviewSettings)
		wantErr string
		pricing *Pricing
	}{
		{"unpriced", func(*ReviewSettings) {}, "", nil},
		{"input and output", func(s *ReviewSettings) { s.PriceInput, s.PriceOutput = price(2), price(10) }, "", &Pricing{Input: 2, CachedInput: 2, CacheWrite: 2, Output: 10}},
		{"cached", func(s *ReviewSettings) {
			s.PriceInput, s.PriceOutput, s.PriceCachedInput = price(2), price(10), price(0.1)
		}, "", &Pricing{Input: 2, CachedInput: 0.1, CacheWrite: 2, Output: 10}},
		{"long context", func(s *ReviewSettings) {
			s.PriceInput, s.PriceOutput = price(2), price(10)
			s.LongContextTokens, s.LongContextInputMultiplier, s.LongContextOutputMultiplier = 272000, 2, 1.5
		}, "", &Pricing{Input: 2, CachedInput: 2, CacheWrite: 2, Output: 10, LongContextTokens: 272000, LongContextInput: 2, LongContextOutput: 1.5}},
		{"input only", func(s *ReviewSettings) { s.PriceInput = price(2) }, "both the input and the output", nil},
		{"cached alone", func(s *ReviewSettings) { s.PriceCachedInput = price(1) }, "need input and output prices", nil},
		{"negative", func(s *ReviewSettings) { s.PriceInput, s.PriceOutput = price(-1), price(1) }, "between 0", nil},
		{"multiplier without threshold", func(s *ReviewSettings) {
			s.PriceInput, s.PriceOutput, s.LongContextInputMultiplier = price(2), price(10), 2
		}, "need a long-context threshold", nil},
		{"multiplier below one", func(s *ReviewSettings) {
			s.PriceInput, s.PriceOutput = price(2), price(10)
			s.LongContextTokens, s.LongContextInputMultiplier, s.LongContextOutputMultiplier = 1000, 0.5, 1
		}, "between 1 and 10", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := base
			test.edit(&settings)
			err := settings.Validate()
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := settings.Profile().Pricing
			if (got == nil) != (test.pricing == nil) || (got != nil && *got != *test.pricing) {
				t.Fatalf("pricing = %+v, want %+v", got, test.pricing)
			}
		})
	}
}

func TestSpendLabelsAndPercentile(t *testing.T) {
	for _, test := range []struct {
		spend Spend
		calls int64
		want  string
	}{
		{Spend{}, 0, "-"},
		{Spend{Unpriced: 3}, 3, "unpriced"},
		{Spend{USD: 0.0123}, 2, "$0.0123"},
		{Spend{USD: 12.345}, 2, "$12.35"},
		{Spend{USD: 0.5, Unpriced: 1}, 3, "$0.5000 + 1 call unpriced"},
	} {
		if got := test.spend.Label(test.calls); got != test.want {
			t.Errorf("Label(%+v, %d) = %q, want %q", test.spend, test.calls, got, test.want)
		}
	}
	values := []float64{5, 1, 4, 2, 3, 10, 6, 7, 8, 9}
	if Percentile(values, 50) != 5 || Percentile(values, 90) != 9 || Percentile(values, 100) != 10 || Percentile(nil, 90) != 0 {
		t.Fatal("percentile")
	}
	if spend, ok := MetricsSpend(map[string]any{"cost_usd": 0.25, "cost_unpriced_calls": float64(2)}); !ok || spend != (Spend{USD: 0.25, Unpriced: 2}) {
		t.Fatalf("metrics spend = %+v %v", spend, ok)
	}
	if _, ok := MetricsSpend(map[string]any{"cost_usd": math.NaN()}); ok {
		t.Fatal("NaN cost accepted")
	}
}

func TestAnthropicSettings(t *testing.T) {
	base := ReviewSettings{Name: "sonnet", Provider: ProviderAnthropic, ConnectionKind: "hosted", BaseURL: "https://api.anthropic.com", Model: "claude-sonnet-5-5", APIKeyEnv: "OVERLOAD_MODEL_ANTHROPIC", PromptProfile: "context", ReasoningParam: ReasoningEffortField, ReasoningEffort: "high"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*ReviewSettings){
		"local":         func(s *ReviewSettings) { s.ConnectionKind = "local" },
		"responses API": func(s *ReviewSettings) { s.API = APIResponses },
		"chat template": func(s *ReviewSettings) { s.ReasoningParam, s.ReasoningEffort = ReasoningChatTemplate, "high" },
		"bad effort":    func(s *ReviewSettings) { s.ReasoningEffort = "minimal" },
		"other":         func(s *ReviewSettings) { s.Provider = "bedrock" },
	} {
		settings := base
		edit(&settings)
		if settings.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
