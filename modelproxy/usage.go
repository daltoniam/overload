package modelproxy

import (
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

// maxEventLine bounds one server-sent event line kept to read usage; a
// longer line (a large content chunk) is skipped, and reading goes on.
const maxEventLine = 1 << 20

// usageMeter reads a model answer's usage as it streams through the
// proxy. Server-sent events are read line by line, so no answer is too
// long to meter. A JSON body is kept up to maxMeteredBytes; past that, or
// when no usage is found, usage is estimated from the answer's size.
type usageMeter struct {
	provider string
	stream   bool
	bytes    int64
	body     bytes.Buffer
	line     []byte
	skipping bool
	total    wireUsage
	found    bool
}

func newUsageMeter(provider, contentType string) *usageMeter {
	return &usageMeter{provider: provider, stream: strings.Contains(contentType, "event-stream")}
}

func (meter *usageMeter) write(data []byte) {
	meter.bytes += int64(len(data))
	if !meter.stream {
		if meter.body.Len() < maxMeteredBytes {
			meter.body.Write(data[:min(len(data), maxMeteredBytes-meter.body.Len())])
		}
		return
	}
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		chunk := data
		if newline >= 0 {
			chunk = data[:newline]
		}
		if !meter.skipping {
			if len(meter.line)+len(chunk) > maxEventLine {
				meter.skipping, meter.line = true, meter.line[:0]
			} else {
				meter.line = append(meter.line, chunk...)
			}
		}
		if newline < 0 {
			return
		}
		if !meter.skipping {
			meter.event(string(meter.line))
		}
		meter.line, meter.skipping = meter.line[:0], false
		data = data[newline+1:]
	}
}

func (meter *usageMeter) event(line string) {
	data, ok := strings.CutPrefix(strings.TrimSuffix(line, "\r"), "data:")
	if !ok {
		return
	}
	var envelope usageEnvelope
	if json.Unmarshal([]byte(strings.TrimSpace(data)), &envelope) != nil || envelope.usage() == nil {
		return
	}
	meter.total.merge(*envelope.usage())
	meter.found = true
}

// estimatedCharsPerToken turns an answer's size into output tokens when
// its usage cannot be read; JSON framing makes it an overestimate.
const estimatedCharsPerToken = 3

// usage returns the answer's usage and whether it was read rather than
// estimated.
func (meter *usageMeter) usage() (overload.Usage, bool) {
	if meter.stream {
		if len(meter.line) > 0 && !meter.skipping {
			meter.event(string(meter.line))
		}
		if meter.found {
			return meter.total.toUsage(meter.provider), true
		}
	} else if meter.bytes <= maxMeteredBytes {
		if usage, ok := parseUsage(meter.provider, "application/json", meter.body.Bytes()); ok {
			return usage, true
		}
	}
	return overload.Usage{Calls: 1, Output: meter.bytes/estimatedCharsPerToken + 1}, false
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
	meter := newUsageMeter(provider, contentType)
	meter.write(body)
	if usage, exact := meter.usage(); exact {
		return usage, true
	}
	return overload.Usage{}, false
}
