package review

import (
	"regexp"
	"slices"
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
	relativeURL  = regexp.MustCompile(`(?i)(^|[\s(<\[])//[a-z0-9-]+(?:\.[a-z0-9-]+)+\S*`)
	referenceDef = regexp.MustCompile(`(?m)^[ \t]*\[[^\]\n]+\]:[^\n]*$`)
	referenceUse = regexp.MustCompile(`\[([^\]\n]*)\]\[[^\]\n]*\]`)
	htmlEntity   = regexp.MustCompile(`(?i)&(?:#[0-9]+|#x[0-9a-f]+|[a-z][a-z0-9]*);`)
)

func Validate(findings []overload.Finding, patch string, limit int) ([]overload.Finding, error) {
	files, err := diff.Parse(patch)
	if err != nil {
		return nil, err
	}
	var valid []overload.Finding
	seen := make(map[string]int)
	for _, finding := range findings {
		finding.Side = strings.ToUpper(strings.TrimSpace(finding.Side))
		if finding.Side == "" {
			finding.Side = "RIGHT"
		}
		if !diff.Commentable(files, finding.Path, finding.Line) || finding.Side != "RIGHT" || finding.Confidence < 0 || finding.Confidence > 1 {
			continue
		}
		var ok bool
		if finding.Severity, ok = normalizeSeverity(finding.Severity); !ok {
			continue
		}
		finding.Category = normalizeCategory(finding.Category)
		if finding.Evidence == "" || !evidenceMatches(files, finding) {
			continue
		}
		key := dedupeKey(finding)
		if index, ok := seen[key]; ok {
			valid[index].Agents = addAgents(valid[index].Agents, finding.Agents)
			continue
		}
		finding.Title = sanitize(finding.Title, 120)
		finding.Body = sanitize(finding.Body, 2000)
		if finding.Title == "" || finding.Body == "" {
			continue
		}
		seen[key] = len(valid)
		finding.Agents = addAgents(nil, finding.Agents)
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

// addAgents appends names not already present, keeping first-seen order.
func addAgents(agents, more []string) []string {
	for _, name := range more {
		if name != "" && !slices.Contains(agents, name) {
			agents = append(agents, name)
		}
	}
	return agents
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

// Severities and categories a finding is stored and posted with.
var (
	Severities = []string{"critical", "high", "medium", "low"}
	Categories = []string{"bug", "security", "performance", "correctness", "maintainability", "test"}
)

// severityWords maps the other words models use for a severity.
var severityWords = map[string]string{"blocker": "critical", "severe": "critical", "major": "high", "error": "high", "moderate": "medium", "warning": "medium", "minor": "low", "info": "low", "trivial": "low", "nit": "low"}

// normalizeSeverity reads a model's severity case-insensitively, accepting
// common synonyms. An unknown severity is rejected: it decides whether a
// review blocks, so it is not guessed.
func normalizeSeverity(severity string) (string, bool) {
	severity = strings.ToLower(strings.TrimSpace(severity))
	if allowed(severity, Severities) {
		return severity, true
	}
	mapped, ok := severityWords[severity]
	return mapped, ok
}

// categoryWords maps the other words models use for a category.
var categoryWords = map[string]string{"vulnerability": "security", "injection": "security", "auth": "security", "perf": "performance", "efficiency": "performance", "testing": "test", "tests": "test", "readability": "maintainability", "style": "maintainability", "design": "maintainability", "logic": "correctness", "arithmetic": "correctness", "off-by-one": "correctness", "concurrency": "correctness", "race": "correctness", "error handling": "correctness", "error-handling": "correctness", "validation": "correctness", "reliability": "correctness"}

// normalizeCategory reads a model's category case-insensitively. A
// category outside the known set is only a label, so the finding is kept
// as a bug rather than dropped.
func normalizeCategory(category string) string {
	category = strings.ToLower(strings.TrimSpace(category))
	if allowed(category, Categories) {
		return category
	}
	if mapped, ok := categoryWords[category]; ok {
		return mapped
	}
	return "bug"
}

// dedupeKey collapses near-identical findings within one review: same file,
// title and category within a five-line window.
func dedupeKey(finding overload.Finding) string {
	return strings.Join([]string{finding.Path, strings.ToLower(strings.TrimSpace(finding.Title)), finding.Category, strconv.Itoa(finding.Line / 5)}, "|")
}

// SeverityRank orders severities from critical (0) to low (3); unknown
// values sort last.
func SeverityRank(severity string) int {
	return rank(severity)
}

func rank(severity string) int {
	for index, value := range Severities {
		if value == severity {
			return index
		}
	}
	return 4
}

// Sanitize strips links, images, HTML and mentions from untrusted text and
// bounds it to limit bytes, as findings are before they are stored.
func Sanitize(value string, limit int) string {
	return sanitize(value, limit)
}

// sanitize prepares model text that may be posted to GitHub as the bot. The
// model reads untrusted PR content, so links, images, HTML and mentions are
// removed rather than trusted.
func sanitize(value string, limit int) string {
	value = htmlEntity.ReplaceAllString(value, "")
	value = htmlTag.ReplaceAllString(value, "")
	value = markdownImg.ReplaceAllString(value, "")
	value = markdownLink.ReplaceAllString(value, "$1")
	value = referenceDef.ReplaceAllString(value, "")
	value = referenceUse.ReplaceAllString(value, "$1")
	value = bareURL.ReplaceAllString(value, "[link removed]")
	value = relativeURL.ReplaceAllString(value, "$1[link removed]")
	value = mention.ReplaceAllStringFunc(value, func(text string) string { return "@ " + text[1:] })
	return overload.TruncateUTF8(strings.TrimSpace(value), limit)
}
