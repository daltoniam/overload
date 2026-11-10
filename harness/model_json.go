package harness

import (
	"cmp"
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

// escapeControlCharacters escapes raw control characters inside JSON
// strings, leaving the rest of the text alone.
func escapeControlCharacters(text string) string {
	var out strings.Builder
	inString, escaped := false, false
	for _, char := range text {
		switch {
		case escaped:
			escaped = false
		case inString && char == '\\':
			escaped = true
		case char == '"':
			inString = !inString
		case inString && char < 0x20:
			switch char {
			case '\t':
				out.WriteString(`\t`)
			case '\n':
				out.WriteString(`\n`)
			case '\r':
				out.WriteString(`\r`)
			default:
				fmt.Fprintf(&out, `\u%04x`, char)
			}
			continue
		}
		out.WriteRune(char)
	}
	return out.String()
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
		Prior    []struct {
			ID     json.RawMessage `json:"id"`
			Status string          `json:"status"`
		} `json:"prior"`
	}
	cleaned := cleanModelJSON(text)
	if err := json.Unmarshal([]byte(cleaned), &raw); err != nil {
		// Models often copy a tab or newline from the code into a string
		// unescaped; escaping them is safer than asking for a repair, which
		// can lose parts of the answer.
		if json.Unmarshal([]byte(escapeControlCharacters(cleaned)), &raw) != nil {
			return overload.ReviewResult{}, err
		}
	}
	result := overload.ReviewResult{Summary: raw.Summary, Findings: make([]overload.Finding, 0, len(raw.Findings))}
	for index, item := range raw.Findings {
		finding := item.Finding
		if finding.Evidence == "" {
			finding.Evidence = cmp.Or(item.ExactEvidence, item.ExactEvidence2)
		}
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
	for _, item := range raw.Prior {
		id, err := looseInt(item.ID)
		status := strings.ToLower(strings.TrimSpace(item.Status))
		if err != nil || id < 1 || (status != overload.PriorResolved && status != overload.PriorStillPresent) {
			continue
		}
		result.Prior = append(result.Prior, overload.PriorVerdict{ID: int64(id), Status: status})
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
	// Models sometimes spell the evidence key after the prompt's wording.
	ExactEvidence  string `json:"exact evidence"`
	ExactEvidence2 string `json:"exact_evidence"`
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
