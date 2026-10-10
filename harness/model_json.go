package harness

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
)

func cleanModelJSON(text string) string {
	text = strings.TrimSpace(text)
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
// numbers ("line": "12", "confidence": "0.9" or "85%"); those are accepted
// rather than discarding the whole file's review. Anything that is not a
// number is still an error.
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

func looseConfidence(raw json.RawMessage) (float64, error) {
	text, err := looseNumber(raw)
	if err != nil || text == "" {
		return 0, err
	}
	percent := strings.HasSuffix(text, "%")
	value, err := strconv.ParseFloat(strings.TrimSuffix(text, "%"), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", text)
	}
	if percent {
		value /= 100
	}
	return value, nil
}
