package harness

import (
	"context"
	"net/http"
	"os"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/openai-go/option"
	"github.com/daltoniam/overload"
)

// modelHTTPClient has no response-header timeout. Non-streaming calls send
// no headers until the whole answer, thinking included, is generated, which
// can take longer than the OpenAI SDK's 10-minute default for local models
// and for hosted reasoning models on large files. Each call is still bounded
// by its plan's timeout (local 90 minutes, hosted 15). Anthropic calls stream
// (see run in stream.go), so the same limits apply to them.
var modelHTTPClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 0
	return &http.Client{Transport: transport}
}()

// anthropicPlaceholderKey is a static key, which makes the Anthropic SDK use
// none of the credentials it finds in overload's environment and profile
// files, and not fail the request when it finds none. anthropicAuth
// replaces it with the connection's own key, or removes it.
const anthropicPlaceholderKey = "overload-placeholder"

func newProvider(model overload.ModelProfile) (fantasy.Provider, error) {
	if model.Provider == overload.ProviderAnthropic {
		client := &http.Client{
			Transport: newAnthropicAuth(modelHTTPClient.Transport, os.Getenv(model.APIKeyEnv), model.Headers),
			// The key travels in X-Api-Key, which Go keeps on redirects to
			// other hosts, so never follow one.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		return anthropic.New(anthropic.WithBaseURL(model.BaseURL), anthropic.WithAPIKey(anthropicPlaceholderKey), anthropic.WithHeaders(model.Headers), anthropic.WithHTTPClient(client))
	}
	if model.API == overload.APIResponses {
		options := []openai.Option{openai.WithBaseURL(model.BaseURL), openai.WithAPIKey(os.Getenv(model.APIKeyEnv)), openai.WithHeaders(model.Headers), openai.WithUseResponsesAPI(), openai.WithResponsesAPIFunc(func(string) bool { return true })}
		options = append(options, openai.WithHTTPClient(modelHTTPClient))
		// Fantasy only sends reasoning settings for model names it knows, so
		// set the field on every request instead.
		if model.ReasoningParam == overload.ReasoningEffortField {
			options = append(options, openai.WithSDKOptions(option.WithJSONSet("reasoning.effort", model.ReasoningEffort)))
		}
		return openai.New(options...)
	}
	options := []openaicompat.Option{openaicompat.WithBaseURL(model.BaseURL), openaicompat.WithAPIKey(os.Getenv(model.APIKeyEnv)), openaicompat.WithHeaders(model.Headers), openaicompat.WithHTTPClient(modelHTTPClient)}
	if model.ConnectionKind == "hosted" {
		options = append(options, openaicompat.WithLanguageModelOptions(openai.WithLanguageModelPrepareCallFunc(hostedPrepareCall)))
	}
	return openaicompat.New(options...)
}

// newAgent builds a model client for one agent, applying its headers,
// connection kind, reasoning style and output limit.
func newAgent(ctx context.Context, model overload.ModelProfile, systemPrompt string, extra ...fantasy.AgentOption) (fantasy.Agent, reasoningPlan, error) {
	if err := validateModelHeaders(model.Headers); err != nil {
		return nil, reasoningPlan{}, err
	}
	provider, err := newProvider(model)
	if err != nil {
		return nil, reasoningPlan{}, err
	}
	languageModel, err := provider.LanguageModel(ctx, model.Model)
	if err != nil {
		return nil, reasoningPlan{}, err
	}
	plan := planFor(model)
	return fantasy.NewAgent(languageModel, append(reviewAgentOptions(plan, systemPrompt), extra...)...), plan, nil
}

// Complete sends one prompt to a model with the same connection, reasoning
// and output settings reviews use, and returns the reply text.
func Complete(ctx context.Context, model overload.ModelProfile, systemPrompt, prompt string) (string, error) {
	agent, plan, err := newAgent(ctx, model, systemPrompt)
	if err != nil {
		return "", err
	}
	response, _, err := generateReview(ctx, agent, plan, fantasy.AgentCall{Prompt: prompt})
	if err != nil {
		return "", err
	}
	return response.Response.Content.Text(), nil
}

// anthropicAuth rebuilds each request's headers from an allowlist and adds
// the connection's own key. The Anthropic SDK reads overload's environment
// and profile files whatever key it is given, and would otherwise send
// their credentials, workspace ID or custom headers to the URL a model
// connection names.
type anthropicAuth struct {
	base    http.RoundTripper
	key     string
	own     map[string]bool
	ambient []string
}

// anthropicHeaders are the request headers the Messages API needs.
var anthropicHeaders = map[string]bool{"Accept": true, "Anthropic-Version": true, "Content-Type": true, "Idempotency-Key": true, "User-Agent": true}

// anthropicDefaults are the SDK's own values for allowed headers that
// ANTHROPIC_CUSTOM_HEADERS can override.
var anthropicDefaults = map[string]string{"Accept": "application/json", "Anthropic-Version": "2023-06-01", "Content-Type": "application/json"}

func newAnthropicAuth(base http.RoundTripper, key string, own map[string]string) anthropicAuth {
	auth := anthropicAuth{base: base, key: key, own: map[string]bool{}}
	for name := range own {
		auth.own[http.CanonicalHeaderKey(name)] = true
	}
	for line := range strings.SplitSeq(os.Getenv("ANTHROPIC_CUSTOM_HEADERS"), "\n") {
		name, _, found := strings.Cut(line, ":")
		if name = http.CanonicalHeaderKey(strings.TrimSpace(name)); found && name != "" && !auth.own[name] {
			auth.ambient = append(auth.ambient, name)
		}
	}
	return auth
}

func (auth anthropicAuth) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for name := range request.Header {
		if !anthropicHeaders[name] && !auth.own[name] && !strings.HasPrefix(name, "X-Stainless-") {
			delete(request.Header, name)
		}
	}
	for _, name := range auth.ambient {
		if value, ok := anthropicDefaults[name]; ok {
			request.Header.Set(name, value)
		} else {
			request.Header.Del(name)
		}
	}
	if auth.key != "" {
		request.Header.Set("X-Api-Key", auth.key)
	}
	return auth.base.RoundTrip(request)
}
