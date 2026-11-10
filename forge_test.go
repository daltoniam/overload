package overload

import "testing"

func TestValidForgeRepositoryName(t *testing.T) {
	tests := []struct {
		kind, name string
		want       bool
	}{
		{ForgeGitHub, "owner/repo", true},
		{ForgeGitHub, "group/sub/repo", false},
		{ForgeGitLab, "group/sub/repo", true},
		{ForgeGitLab, "group/repo", true},
		{ForgeGitLab, "repo", false},
		{ForgeGitLab, "group/../repo", false},
		{ForgeGitLab, "group/repo.git", false},
		{ForgeGitLab, "group//repo", false},
		{ForgeForgejo, "owner/repo", true},
		{ForgeForgejo, "owner/sub/repo", false},
		{ForgeForgejo, "owner/re po", false},
		{"bitbucket", "owner/repo", false},
	}
	for _, test := range tests {
		if got := ValidForgeRepositoryName(test.kind, test.name); got != test.want {
			t.Errorf("ValidForgeRepositoryName(%q, %q) = %v", test.kind, test.name, got)
		}
	}
}

func TestForgeBaseURL(t *testing.T) {
	tests := []struct {
		raw, want string
		ok        bool
	}{
		{"https://gitlab.com/", "https://gitlab.com", true},
		{"https://git.example.com/gitlab/", "https://git.example.com/gitlab", true},
		{"http://localhost:3000", "http://localhost:3000", true},
		{"http://gitlab.example.com", "", false},
		{"https://user:pass@gitlab.com", "", false},
		{"https://gitlab.com/?x=1", "", false},
		{"gitlab.com", "", false},
	}
	for _, test := range tests {
		got, err := ForgeBaseURL(test.raw)
		if (err == nil) != test.ok || got != test.want {
			t.Errorf("ForgeBaseURL(%q) = %q, %v", test.raw, got, err)
		}
	}
}
