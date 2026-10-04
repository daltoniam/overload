package review

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/diff"
)

var (
	htmlTag      = regexp.MustCompile(`<[^>]*>`)
	mention      = regexp.MustCompile(`@[a-zA-Z0-9_-]+`)
	markdownImg  = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	bareURL      = regexp.MustCompile(`(?i)\b(?:https?|ftp)://\S+|\bwww\.\S+`)
)

func Validate(findings []overload.Finding, patch string, limit int) ([]overload.Finding, error) {
	files, err := diff.Parse(patch)
	if err != nil {
		return nil, err
	}
	var valid []overload.Finding
	seen := make(map[string]bool)
	for _, finding := range findings {
		if !diff.Commentable(files, finding.Path, finding.Line) || finding.Side != "RIGHT" || finding.Confidence < 0 || finding.Confidence > 1 {
			continue
		}
		if !allowed(finding.Severity, []string{"critical", "high", "medium", "low"}) || !allowed(finding.Category, []string{"bug", "security", "performance", "correctness", "maintainability", "test"}) {
			continue
		}
		if finding.Evidence == "" || !evidenceMatches(files, finding) {
			continue
		}
		key := dedupeKey(finding)
		if seen[key] {
			continue
		}
		finding.Title = sanitize(finding.Title, 120)
		finding.Body = sanitize(finding.Body, 2000)
		if finding.Title == "" || finding.Body == "" {
			continue
		}
		seen[key] = true
		valid = append(valid, finding)
	}
	sort.SliceStable(valid, func(i, j int) bool {
		if rank(valid[i].Severity) != rank(valid[j].Severity) {
			return rank(valid[i].Severity) < rank(valid[j].Severity)
		}
		return valid[i].Confidence > valid[j].Confidence
	})
	if limit >= 0 && len(valid) > limit {
		valid = valid[:limit]
	}
	return valid, nil
}

func allowed(value string, options []string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func evidenceMatches(files []diff.File, finding overload.Finding) bool {
	for _, file := range files {
		if file.Path != finding.Path {
			continue
		}
		for offset := -2; offset <= 2; offset++ {
			if line, ok := file.Lines[finding.Line+offset]; ok && strings.Contains(strings.TrimSpace(line), strings.TrimSpace(finding.Evidence)) {
				return true
			}
		}
	}
	return false
}

// dedupeKey collapses near-identical findings within one review: same file,
// title and category within a five-line window.
func dedupeKey(finding overload.Finding) string {
	return strings.Join([]string{finding.Path, strings.ToLower(strings.TrimSpace(finding.Title)), finding.Category, strconv.Itoa(finding.Line / 5)}, "|")
}

func rank(severity string) int {
	for index, value := range []string{"critical", "high", "medium", "low"} {
		if value == severity {
			return index
		}
	}
	return 4
}

// sanitize prepares model text that may be posted to GitHub as the bot. The
// model reads untrusted PR content, so links, images, HTML and mentions are
// removed rather than trusted.
func sanitize(value string, limit int) string {
	value = htmlTag.ReplaceAllString(value, "")
	value = markdownImg.ReplaceAllString(value, "")
	value = markdownLink.ReplaceAllString(value, "$1")
	value = bareURL.ReplaceAllString(value, "[link removed]")
	value = mention.ReplaceAllStringFunc(value, func(text string) string { return "@ " + text[1:] })
	return overload.TruncateUTF8(strings.TrimSpace(value), limit)
}
