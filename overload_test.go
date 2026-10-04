package overload

import "testing"

func TestReviewSettingsValidate(t *testing.T) {
	valid := ReviewSettings{Name: "bonsai", Provider: "openaicompat", BaseURL: "http://127.0.0.1:8080/v1", Model: "bonsai-2-27b", PromptProfile: "context"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*ReviewSettings)
	}{
		{"secret URL", func(s *ReviewSettings) { s.BaseURL = "http://token:secret@example.com/v1" }},
		{"invalid scheme", func(s *ReviewSettings) { s.BaseURL = "file:///tmp/model" }},
		{"invalid prompt", func(s *ReviewSettings) { s.PromptProfile = "unsafe" }},
		{"invalid provider", func(s *ReviewSettings) { s.Provider = "native" }},
		{"invalid env", func(s *ReviewSettings) { s.APIKeyEnv = "secret-value!" }},
		{"invalid name", func(s *ReviewSettings) { s.Name = "../outside" }},
		{"duplicate agents", func(s *ReviewSettings) {
			s.Agents = []ReviewAgent{{Name: "security", Instructions: "Check auth"}, {Name: "security", Instructions: "Check input"}}
		}},
		{"empty agent prompt", func(s *ReviewSettings) { s.Agents = []ReviewAgent{{Name: "security", Instructions: " "}} }},
		{"too many agents", func(s *ReviewSettings) {
			s.Agents = []ReviewAgent{{Name: "a", Instructions: "a"}, {Name: "b", Instructions: "b"}, {Name: "c", Instructions: "c"}, {Name: "d", Instructions: "d"}, {Name: "e", Instructions: "e"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			setting := valid
			test.change(&setting)
			if err := setting.Validate(); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
}

func TestFindingFingerprint(t *testing.T) {
	base := Finding{Path: "a.go", Line: 10, Category: "bug", Title: "Nil  deref", Evidence: "x.y()"}
	moved := base
	moved.Line = 42
	moved.Title = "nil deref"
	if FindingFingerprint("Acme/API", 1, base) != FindingFingerprint("acme/api", 1, moved) {
		t.Fatal("line or case change altered fingerprint")
	}
	other := base
	other.Path = "b.go"
	if FindingFingerprint("acme/api", 1, base) == FindingFingerprint("acme/api", 1, other) || FindingFingerprint("acme/api", 1, base) == FindingFingerprint("acme/api", 2, base) {
		t.Fatal("distinct findings collided")
	}
}
