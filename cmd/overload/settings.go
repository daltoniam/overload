package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/daltoniam/overload"
)

func manageSettings(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: overload settings list|show|set|delete")
	}
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
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: overload settings list")
		}
		settings, err := store.ListReviewSettings(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(settings)
	case "show":
		if len(args) > 2 {
			return errors.New("usage: overload settings show [name]")
		}
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		setting, err := store.GetReviewSettings(ctx, name)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(setting)
	case "set":
		flags := flag.NewFlagSet("settings set", flag.ContinueOnError)
		name := flags.String("name", "", "profile name")
		provider := flags.String("provider", overload.ProviderOpenAICompat, "openaicompat (any OpenAI-compatible API) or anthropic (Messages API; --url https://api.anthropic.com)")
		baseURL := flags.String("url", "", "base URL: OpenAI-compatible ending in /v1, or https://api.anthropic.com")
		model := flags.String("model", "", "model ID")
		keyEnv := flags.String("api-key-env", "", "environment variable name containing API key")
		connectionKind := flags.String("connection-kind", "local", "local or hosted OpenAI-compatible connection")
		prompt := flags.String("prompt", "context", "context or switchboard-go")
		asDefault := flags.Bool("default", false, "make default")
		reasoningParam := flags.String("reasoning-param", "", "how thinking is requested: empty (auto), none, chat_template or reasoning_effort")
		reasoningEffort := flags.String("reasoning-effort", "", "thinking level for --reasoning-param (none, minimal, low, medium, high, xhigh, max)")
		maxOutputTokens := flags.Int("max-output-tokens", 0, "output token limit per review call, thinking included (0 = default)")
		concurrency := flags.Int("concurrency", 1, "changed files reviewed at once (match the model server's parallel slots)")
		api := flags.String("api", "", "request style: empty for Chat Completions, or responses for OpenAI's Responses API (newer OpenAI models need it for tools with reasoning)")
		priceInput := flags.String("price-input", "", "US dollars per million input tokens (with --price-output; empty leaves calls unpriced)")
		priceCachedInput := flags.String("price-cached-input", "", "US dollars per million cached input tokens (default: the input price)")
		priceCacheWrite := flags.String("price-cache-write", "", "US dollars per million tokens written to the prompt cache (default: the input price)")
		priceOutput := flags.String("price-output", "", "US dollars per million output tokens, thinking included")
		longContextTokens := flags.Int64("long-context-tokens", 0, "prompt tokens in one call above which long-context prices apply (0 = none)")
		longContextInput := flags.Float64("long-context-input-multiplier", 0, "input price multiplier above --long-context-tokens")
		longContextOutput := flags.Float64("long-context-output-multiplier", 0, "output price multiplier above --long-context-tokens")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected setting arguments")
		}
		setting := overload.ReviewSettings{Name: *name, Provider: *provider, ConnectionKind: *connectionKind, BaseURL: *baseURL, Model: *model, APIKeyEnv: *keyEnv, PromptProfile: *prompt, IsDefault: *asDefault, Concurrency: *concurrency, ReasoningParam: *reasoningParam, ReasoningEffort: *reasoningEffort, MaxOutputTokens: *maxOutputTokens, API: *api, LongContextTokens: *longContextTokens, LongContextInputMultiplier: *longContextInput, LongContextOutputMultiplier: *longContextOutput}
		for _, price := range []struct {
			text   string
			target **float64
		}{{*priceInput, &setting.PriceInput}, {*priceCachedInput, &setting.PriceCachedInput}, {*priceCacheWrite, &setting.PriceCacheWrite}, {*priceOutput, &setting.PriceOutput}} {
			value, err := overload.ParsePrice(price.text)
			if err != nil {
				return err
			}
			*price.target = value
		}
		if err := store.SaveReviewSettings(ctx, setting); err != nil {
			return err
		}
		_, err := fmt.Fprintln(os.Stdout, "Settings saved")
		return err
	case "delete":
		if len(args) != 2 {
			return errors.New("usage: overload settings delete NAME")
		}
		return store.DeleteReviewSettings(ctx, args[1])
	default:
		return errors.New("usage: overload settings list|show|set|delete")
	}
}
