package modelproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"github.com/daltoniam/overload"
)

// wireUsage covers the usage fields of OpenAI Chat Completions, OpenAI
// Responses and Anthropic Messages answers.
type wireUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	PromptDetails       struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type usageEnvelope struct {
	Usage    *wireUsage `json:"usage"`
	Response *struct {
		Usage *wireUsage `json:"usage"`
	} `json:"response"`
	Message *struct {
		Usage *wireUsage `json:"usage"`
	} `json:"message"`
}

func (envelope usageEnvelope) usage() *wireUsage {
	switch {
	case envelope.Usage != nil:
		return envelope.Usage
	case envelope.Response != nil && envelope.Response.Usage != nil:
		return envelope.Response.Usage
	case envelope.Message != nil && envelope.Message.Usage != nil:
		return envelope.Message.Usage
	}
	return nil
}

// toUsage converts a provider's usage to overload's, where input counts
// every prompt token. OpenAI's prompt and input counts include cached
// tokens; Anthropic's input count excludes cache reads and writes.
func (usage wireUsage) toUsage(provider string) overload.Usage {
	if provider == overload.ProviderAnthropic {
		return overload.Usage{Calls: 1, Input: usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens, CachedInput: usage.CacheReadTokens, CacheWrite: usage.CacheCreationTokens, Output: usage.OutputTokens}
	}
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
		return overload.Usage{Calls: 1, Input: usage.PromptTokens, CachedInput: usage.PromptDetails.CachedTokens, Output: usage.CompletionTokens}
	}
	return overload.Usage{Calls: 1, Input: usage.InputTokens, CachedInput: usage.InputDetails.CachedTokens, Output: usage.OutputTokens}
}

// merge keeps the largest count of each field: streamed answers repeat or
// grow their usage as they go, and Anthropic reports input at the start
// and output at the end.
func (usage *wireUsage) merge(other wireUsage) {
	usage.PromptTokens = max(usage.PromptTokens, other.PromptTokens)
	usage.CompletionTokens = max(usage.CompletionTokens, other.CompletionTokens)
	usage.InputTokens = max(usage.InputTokens, other.InputTokens)
	usage.OutputTokens = max(usage.OutputTokens, other.OutputTokens)
	usage.CacheReadTokens = max(usage.CacheReadTokens, other.CacheReadTokens)
	usage.CacheCreationTokens = max(usage.CacheCreationTokens, other.CacheCreationTokens)
	usage.PromptDetails.CachedTokens = max(usage.PromptDetails.CachedTokens, other.PromptDetails.CachedTokens)
	usage.InputDetails.CachedTokens = max(usage.InputDetails.CachedTokens, other.InputDetails.CachedTokens)
}

// parseUsage reads a model answer's usage from a JSON body or a stream of
// server-sent events. It returns false when the answer has none.
func parseUsage(provider, contentType string, body []byte) (overload.Usage, bool) {
	if !strings.Contains(contentType, "event-stream") {
		var envelope usageEnvelope
		if json.Unmarshal(body, &envelope) != nil || envelope.usage() == nil {
			return overload.Usage{}, false
		}
		return envelope.usage().toUsage(provider), true
	}
	var total wireUsage
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		var envelope usageEnvelope
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &envelope) != nil || envelope.usage() == nil {
			continue
		}
		total.merge(*envelope.usage())
		found = true
	}
	if !found {
		return overload.Usage{}, false
	}
	return total.toUsage(provider), true
}
