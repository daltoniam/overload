package overload

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// Git hosts overload reviews pull requests on. GitHub connects through a
// GitHub App or a token; GitLab and Forgejo (and Gitea) connect through a
// ForgeConnection with an access token.
const (
	ForgeGitHub  = "github"
	ForgeGitLab  = "gitlab"
	ForgeForgejo = "forgejo"
)

// PullRequest is a pull request (a GitLab merge request) as a git host
// reports it, reduced to what a review needs.
type PullRequest struct {
	Number int
	// State is "open" for a pull request that can still be reviewed.
	State   string
	Draft   bool
	HeadSHA string
	BaseSHA string
	// RepositoryID is the host's numeric ID for the repository the pull
	// request targets, used to follow renames.
	RepositoryID int64
}

// Errors a git host client returns that the posting worker handles.
var (
	// ErrReviewRejected means the host refused the review itself, for
	// example because the commit is no longer part of the pull request
	// after a force push. Posting the same review again would fail the
	// same way.
	ErrReviewRejected = errors.New("the git host rejected the review")
	// ErrStatusForbidden means the credentials may not set commit statuses.
	ErrStatusForbidden = errors.New("not allowed to set commit statuses")
)

// ForgeConnection is a GitLab or Forgejo server overload reads pull
// requests from and posts reviews to.
type ForgeConnection struct {
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// BaseURL is the server's web address, for example https://gitlab.com
	// or https://codeberg.org.
	BaseURL string `json:"base_url"`
	// Token is the access token overload acts with. It is never written
	// out by list commands or pages.
	Token string `json:"-"`
	// WebhookSecret signs (Forgejo) or authenticates (GitLab) webhook
	// deliveries; overload generates it.
	WebhookSecret string `json:"-"`
}

var forgeKinds = map[string]bool{ForgeGitLab: true, ForgeForgejo: true}

// Validate checks a connection before it is saved.
func (connection ForgeConnection) Validate() error {
	if !settingName.MatchString(connection.Name) {
		return errors.New("invalid name: use letters, digits, - and _")
	}
	if !forgeKinds[connection.Kind] {
		return errors.New("kind must be gitlab or forgejo")
	}
	if _, err := ForgeBaseURL(connection.BaseURL); err != nil {
		return err
	}
	if strings.TrimSpace(connection.Token) == "" || strings.ContainsAny(connection.Token, " \r\n\t") {
		return errors.New("an access token is required")
	}
	return nil
}

// ForgeBaseURL normalizes a git host's web address: https (http only for
// localhost), no credentials, query or fragment, no trailing slash.
func ForgeBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid base URL: use the server's address, for example https://gitlab.com")
	}
	host := parsed.Hostname()
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !local) {
		return "", errors.New("base URL must use https")
	}
	return strings.TrimRight(parsed.Scheme+"://"+parsed.Host+parsed.EscapedPath(), "/"), nil
}

var forgePathSegment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// ValidForgeRepositoryName reports whether name is a repository path on
// a host of the given kind: owner/name on GitHub and Forgejo, and
// group/subgroup/.../name on GitLab.
func ValidForgeRepositoryName(kind, name string) bool {
	switch kind {
	case "", ForgeGitHub:
		return ValidRepositoryName(name)
	case ForgeForgejo:
		return strings.Count(name, "/") == 1 && validPathSegments(name)
	case ForgeGitLab:
		return len(name) <= 255 && validPathSegments(name)
	}
	return false
}

func validPathSegments(name string) bool {
	segments := strings.Split(name, "/")
	if len(segments) < 2 || len(segments) > 21 {
		return false
	}
	for _, segment := range segments {
		if !forgePathSegment.MatchString(segment) || strings.HasSuffix(segment, ".git") {
			return false
		}
	}
	return true
}
