package harness

import "strings"

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
