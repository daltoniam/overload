package review

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/daltoniam/overload"
)

func TestValidateRejectsInvalidAndSanitizes(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+dangerous()\n"
	finding := overload.Finding{Path: "a.go", Line: 1, Side: "RIGHT", Severity: "high", Category: "bug", Title: "<b>Bug</b>", Body: "@someone please check", Evidence: "dangerous()", Confidence: 0.9}
	bad := finding
	bad.Line = 10
	valid, err := Validate([]overload.Finding{bad, finding, finding}, patch, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 || strings.Contains(valid[0].Body, "@someone") || strings.Contains(valid[0].Title, "<") {
		t.Fatalf("unexpected validated findings: %+v", valid)
	}
}

func TestSanitizeStripsLinksAndKeepsUTF8(t *testing.T) {
	text := sanitize("See [docs](https://evil.example/x) ![beacon](https://evil.example/p.png) https://evil.example/raw www.evil.example and @octocat <b>bold</b>", 2000)
	if strings.Contains(text, "evil.example") || strings.Contains(text, "@octocat") || strings.Contains(text, "<b>") || !strings.Contains(text, "See docs") {
		t.Fatalf("unsafe text kept: %q", text)
	}
	cut := sanitize(strings.Repeat("é", 100), 121)
	if !utf8.ValidString(cut) || len(cut) != 120 {
		t.Fatalf("truncation split a character: %d bytes valid=%v", len(cut), utf8.ValidString(cut))
	}
}

func TestValidateMergesAgentsOfDuplicateFindings(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@\n-old\n+dangerous()\n+second()\n"
	base := overload.Finding{Path: "a.go", Line: 1, Side: "RIGHT", Severity: "high", Category: "bug", Title: "Unsafe call", Body: "Fix it", Evidence: "dangerous()", Confidence: 0.9}
	lead, security, again := base, base, base
	lead.Agents = []string{"lead"}
	security.Agents = []string{"security"}
	security.Line = 2
	security.Evidence = "second()"
	again.Agents = []string{"lead"}
	valid, err := Validate([]overload.Finding{lead, security, again}, patch, 10)
	if err != nil || len(valid) != 1 {
		t.Fatalf("valid=%+v err=%v", valid, err)
	}
	if got := strings.Join(valid[0].Agents, ","); got != "lead,security" {
		t.Fatalf("agents %q, want lead,security", got)
	}
}

func TestSanitizeStripsReferenceLinksRelativeURLsAndEntities(t *testing.T) {
	text := sanitize("Read [the guide][g] or (//evil.example/x).\n[g]: //evil.example/guide\nPing &#64;octocat and &commat;admin. Use a // comment in code.", 2000)
	for _, bad := range []string{"evil.example", "&#64;", "@octocat", "&commat;", "[g]"} {
		if strings.Contains(text, bad) {
			t.Fatalf("%q survived: %q", bad, text)
		}
	}
	if !strings.Contains(text, "Read the guide") || !strings.Contains(text, "Use a // comment in code.") {
		t.Fatalf("plain text damaged: %q", text)
	}
}
