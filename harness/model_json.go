package harness

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
)

// stripThinking drops reasoning a server left inline in the answer as a
// <think>...</think> block, as Recursive Flint and other always-thinking
// models do when the server has no reasoning parser for their template. A
// lone </think> counts too, for templates that open the block in the prompt,
// unless the reply already starts as JSON or fenced JSON: then the tag is
// quoted inside a finding and the reply is kept.
func stripThinking(text string) string {
	text = strings.TrimSpace(text)
	end := strings.Index(text, "</think>")
	if end < 0 {
		if strings.HasPrefix(text, "<think>") {
			return ""
		}
		return text
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "```") {
		return text
	}
	return strings.TrimSpace(text[end+len("</think>"):])
}

func cleanModelJSON(text string) string {
	text = stripThinking(text)
	if !strings.HasPrefix(text, "```") {
		return text
	}
	newline := strings.IndexByte(text, '\n')
	if newline < 0 || !strings.HasSuffix(text, "```") {
		return text
	}
	language := strings.TrimSpace(text[3:newline])
	if language != "" && !strings.EqualFold(language, "json") {
		return text
	}
	return strings.TrimSpace(text[newline+1 : len(text)-3])
}

// decodeModelReview parses a model's review reply. Models sometimes quote
// numbers ("line": "12", "confidence": "0.9" or "85%") or describe
// confidence in words ("high"); those are accepted rather than discarding
// the whole file's review. A line that is not a number is still an error,
// because findings are placed on lines.
func decodeModelReview(text string) (overload.ReviewResult, error) {
	var raw struct {
		Summary  string         `json:"summary"`
		Findings []looseFinding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(cleanModelJSON(text)), &raw); err != nil {
		return overload.ReviewResult{}, err
	}
	result := overload.ReviewResult{Summary: raw.Summary, Findings: make([]overload.Finding, 0, len(raw.Findings))}
	for index, item := range raw.Findings {
		finding := item.Finding
		var err error
		if finding.Line, err = looseInt(item.Line); err != nil {
			return overload.ReviewResult{}, fmt.Errorf("finding %d line: %w", index, err)
		}
		if finding.StartLine, err = looseInt(item.StartLine); err != nil {
			return overload.ReviewResult{}, fmt.Errorf("finding %d start_line: %w", index, err)
		}
		if finding.Confidence, err = looseConfidence(item.Confidence); err != nil {
			return overload.ReviewResult{}, fmt.Errorf("finding %d confidence: %w", index, err)
		}
		result.Findings = append(result.Findings, finding)
	}
	return result, nil
}

// looseFinding reads the numeric fields as raw JSON so quoted numbers can
// be converted; the embedded Finding's own fields are shadowed.
type looseFinding struct {
	overload.Finding
	Line       json.RawMessage `json:"line"`
	StartLine  json.RawMessage `json:"start_line"`
	Confidence json.RawMessage `json:"confidence"`
}

func looseNumber(raw json.RawMessage) (string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return "", nil
	}
	if strings.HasPrefix(text, `"`) {
		var quoted string
		if err := json.Unmarshal(raw, &quoted); err != nil {
			return "", err
		}
		text = strings.TrimSpace(quoted)
	}
	return text, nil
}

func looseInt(raw json.RawMessage) (int, error) {
	text, err := looseNumber(raw)
	if err != nil || text == "" {
		return 0, err
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%q is not a whole number", text)
	}
	return value, nil
}

// unknownConfidence is used when a model's confidence cannot be read.
// Confidence only orders findings, so an unreadable value never fails a
// review.
const unknownConfidence = 0.5

var confidenceWords = map[string]float64{
	"very high": 0.95, "certain": 0.95, "high": 0.8, "medium": 0.5, "moderate": 0.5,
	"low": 0.3, "very low": 0.1,
}

func looseConfidence(raw json.RawMessage) (float64, error) {
	text, err := looseNumber(raw)
	if err != nil {
		return unknownConfidence, nil
	}
	if text == "" {
		return 0, nil
	}
	if value, ok := confidenceWords[strings.Join(strings.Fields(strings.ToLower(text)), " ")]; ok {
		return value, nil
	}
	percent := strings.HasSuffix(text, "%")
	value, err := strconv.ParseFloat(strings.TrimSuffix(text, "%"), 64)
	if err != nil {
		return unknownConfidence, nil
	}
	if percent {
		value /= 100
	}
	if value < 0 || value > 1 {
		return unknownConfidence, nil
	}
	return value, nil
}
