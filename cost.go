package overload

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Pricing is what a model costs, in US dollars per million tokens.
// CachedInput is the price of prompt tokens read from the provider's cache
// and CacheWrite the price of tokens written to it (Anthropic charges
// extra for writes; OpenAI caches for free). Above LongContextTokens prompt
// tokens in one call, input prices (cached and cache writes included) are
// multiplied by LongContextInput and the output price by
// LongContextOutput.
type Pricing struct {
	Input             float64 `json:"input"`
	CachedInput       float64 `json:"cached_input"`
	CacheWrite        float64 `json:"cache_write"`
	Output            float64 `json:"output"`
	LongContextTokens int64   `json:"long_context_tokens,omitempty"`
	LongContextInput  float64 `json:"long_context_input,omitempty"`
	LongContextOutput float64 `json:"long_context_output,omitempty"`
}

// Usage is the tokens one or more model calls used. Input counts every
// prompt token, including the CachedInput tokens read from the provider's
// cache and the CacheWrite tokens written to it.
type Usage struct {
	Calls       int64 `json:"calls,omitempty"`
	Input       int64 `json:"input_tokens"`
	CachedInput int64 `json:"cached_input_tokens,omitempty"`
	CacheWrite  int64 `json:"cache_write_tokens,omitempty"`
	Output      int64 `json:"output_tokens"`
}

// Add returns the sum of two usages.
func (usage Usage) Add(other Usage) Usage {
	return Usage{Calls: usage.Calls + other.Calls, Input: usage.Input + other.Input, CachedInput: usage.CachedInput + other.CachedInput, CacheWrite: usage.CacheWrite + other.CacheWrite, Output: usage.Output + other.Output}
}

// Cost prices one model call. It returns false when the model has no
// prices, which callers show as "unpriced", never as $0.
func (pricing *Pricing) Cost(call Usage) (float64, bool) {
	if pricing == nil {
		return 0, false
	}
	inputRate, outputRate := 1.0, 1.0
	if pricing.LongContextTokens > 0 && call.Input > pricing.LongContextTokens {
		inputRate, outputRate = max(pricing.LongContextInput, 1), max(pricing.LongContextOutput, 1)
	}
	uncached := max(call.Input-call.CachedInput-call.CacheWrite, 0)
	usd := (float64(uncached)*pricing.Input + float64(call.CachedInput)*pricing.CachedInput + float64(call.CacheWrite)*pricing.CacheWrite) * inputRate
	usd += float64(call.Output) * pricing.Output * outputRate
	return usd / 1e6, true
}

// Spend is the priced cost of some model calls. Unpriced counts calls to
// models without prices, which are not in USD.
type Spend struct {
	USD      float64 `json:"usd"`
	Unpriced int64   `json:"unpriced_calls,omitempty"`
}

// Add returns the sum of two spends.
func (spend Spend) Add(other Spend) Spend {
	return Spend{USD: spend.USD + other.USD, Unpriced: spend.Unpriced + other.Unpriced}
}

// Charge adds one call's cost under pricing.
func (spend Spend) Charge(pricing *Pricing, call Usage) Spend {
	if usd, ok := pricing.Cost(call); ok {
		spend.USD += usd
	} else {
		spend.Unpriced++
	}
	return spend
}

// FormatUSD shows a dollar amount: cents above $1, otherwise four decimals,
// so a cheap file review does not read as $0.00.
func FormatUSD(usd float64) string {
	if math.Abs(usd) >= 1 {
		return "$" + strconv.FormatFloat(usd, 'f', 2, 64)
	}
	return "$" + strconv.FormatFloat(usd, 'f', 4, 64)
}

// Label describes a spend for people: "unpriced" when no call had prices,
// the amount otherwise, noting calls left out.
func (spend Spend) Label(calls int64) string {
	switch {
	case spend.Unpriced > 0 && spend.Unpriced >= calls && spend.USD == 0:
		return "unpriced"
	case spend.Unpriced > 0:
		return fmt.Sprintf("%s + %s unpriced", FormatUSD(spend.USD), Plural(int(spend.Unpriced), "call"))
	case calls == 0:
		return "-"
	}
	return FormatUSD(spend.USD)
}

const maxPrice = 10000

// validatePricing checks optional prices: input and output go together,
// prices are between 0 and maxPrice per million tokens, and a long-context
// threshold comes with multipliers of at least 1.
func validatePricing(input, cachedInput, cacheWrite, output *float64, longTokens int64, longInput, longOutput float64) error {
	for _, price := range []*float64{input, cachedInput, cacheWrite, output} {
		if price != nil && (math.IsNaN(*price) || *price < 0 || *price > maxPrice) {
			return fmt.Errorf("prices must be between 0 and %d dollars per million tokens", maxPrice)
		}
	}
	if (input == nil) != (output == nil) {
		return errors.New("set both the input and the output price, or neither")
	}
	if input == nil && (cachedInput != nil || cacheWrite != nil || longTokens != 0) {
		return errors.New("cached, cache-write and long-context prices need input and output prices")
	}
	switch {
	case longTokens < 0:
		return errors.New("the long-context threshold must be positive")
	case longTokens == 0 && (longInput != 0 || longOutput != 0):
		return errors.New("long-context multipliers need a long-context threshold")
	case longTokens > 0 && (longInput < 1 || longInput > 10 || longOutput < 1 || longOutput > 10):
		return errors.New("long-context multipliers must be between 1 and 10")
	}
	return nil
}

// Percentile returns the p-th percentile (0 to 100) of values by the
// nearest-rank method, or 0 for none.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// ParsePrice reads an optional price flag: "" means unset.
func ParsePrice(text string) (*float64, error) {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "$"))
	if text == "" {
		return nil, nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("invalid price %q", text)
	}
	return &value, nil
}

// MetricsSpend reads a review's spend from its metrics, whether they were
// built in this process or decoded from a sandbox's JSON result.
func MetricsSpend(metrics map[string]any) (Spend, bool) {
	usd, ok := metricNumber(metrics["cost_usd"])
	if !ok {
		return Spend{}, false
	}
	unpriced, _ := metricNumber(metrics["cost_unpriced_calls"])
	return Spend{USD: usd, Unpriced: int64(unpriced)}, true
}

func metricNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, !math.IsNaN(number) && !math.IsInf(number, 0)
	case int64:
		return float64(number), true
	case int:
		return float64(number), true
	}
	return 0, false
}
