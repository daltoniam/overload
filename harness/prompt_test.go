package harness

import (
	"strings"
	"testing"
)

func TestPromptProfiles(t *testing.T) {
	for _, test := range []struct {
		profile  string
		version  string
		contains string
		valid    bool
	}{
		{"", "context-v1", "untrusted data", true},
		{"context", "context-v1", "untrusted data", true},
		{"switchboard-go", "switchboard-go-v1", "compaction", true},
		{"unknown", "", "", false},
	} {
		t.Run(test.profile, func(t *testing.T) {
			prompt, version, err := loadPrompt(test.profile)
			if (err == nil) != test.valid || version != test.version || (test.valid && !strings.Contains(prompt, test.contains)) {
				t.Fatalf("profile=%q version=%q error=%v", prompt, version, err)
			}
		})
	}
}
