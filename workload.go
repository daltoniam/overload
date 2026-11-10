package overload

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// GitHubActionsIssuer issues GitHub Actions OIDC tokens.
const GitHubActionsIssuer = "https://token.actions.githubusercontent.com"

// IsGitHubActionsIssuer reports whether issuer is GitHub Actions, including
// an enterprise's own issuer under it (GitHubActionsIssuer/<slug>), where
// every job of every repository can get a token just the same.
func IsGitHubActionsIssuer(issuer string) bool {
	return issuer == GitHubActionsIssuer || strings.HasPrefix(issuer, GitHubActionsIssuer+"/")
}

// ErrWorkloadRuleExists means a new rule reuses an existing rule's name.
var ErrWorkloadRuleExists = errors.New("a workload rule with this name already exists")

// WorkloadRule trusts OIDC tokens from automation, such as GitHub Actions
// jobs or Kubernetes service accounts, whose claims match Conditions. Each
// condition is a claim name and a pattern (path.Match syntax, so `*` does
// not cross `/`); all must match.
type WorkloadRule struct {
	Name       string            `json:"name"`
	Issuer     string            `json:"issuer"`
	Audience   string            `json:"audience"`
	Conditions map[string]string `json:"conditions"`
	Scopes     []string          `json:"scopes"`
	Enabled    bool              `json:"enabled"`
}

// Validate checks a rule before it is saved. A rule must narrow the tokens
// it trusts: GitHub rules must pin one owner, since every GitHub Actions
// job anywhere can get a token from the same issuer.
func (rule WorkloadRule) Validate() error {
	if !settingName.MatchString(rule.Name) {
		return errors.New("invalid rule name")
	}
	// The rule form lives at /configure/workloads/new.
	if rule.Name == "new" {
		return errors.New(`"new" is reserved; choose another rule name`)
	}
	parsed, err := url.Parse(rule.Issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return errors.New("the issuer must be an https URL")
	}
	if strings.TrimSpace(rule.Audience) == "" || len(rule.Audience) > 300 {
		return errors.New("set the audience the tokens are issued for")
	}
	if len(rule.Conditions) == 0 || len(rule.Conditions) > 20 {
		return errors.New("add 1 to 20 claim conditions")
	}
	for claim, pattern := range rule.Conditions {
		if !settingName.MatchString(claim) || pattern == "" || len(pattern) > 300 {
			return fmt.Errorf("invalid condition %q", claim)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("condition %s: invalid pattern", claim)
		}
	}
	if IsGitHubActionsIssuer(rule.Issuer) && !rule.pinsGitHubOwner() {
		return errors.New("GitHub Actions rules must name one owner without wildcards: match repository_owner_id, repository_id, repository_owner or repository (owner/name)")
	}
	if len(rule.Scopes) == 0 {
		return errors.New("choose at least one scope")
	}
	for _, scope := range rule.Scopes {
		if scope != ScopeModels && scope != ScopeReviews {
			return errors.New("scopes are models and reviews")
		}
	}
	return nil
}

// pinsGitHubOwner reports whether a condition limits the rule to a single
// repository owner. Wildcards in the owner part, as in `*/*` or
// repository_owner=`*`, would trust every GitHub repository.
func (rule WorkloadRule) pinsGitHubOwner() bool {
	for _, claim := range []string{"repository_owner_id", "repository_id", "repository_owner"} {
		if pattern := rule.Conditions[claim]; pattern != "" && literal(pattern) {
			return true
		}
	}
	owner, _, _ := strings.Cut(rule.Conditions["repository"], "/")
	return owner != "" && literal(owner)
}

// literal reports whether a pattern matches only itself.
func literal(pattern string) bool { return !strings.ContainsAny(pattern, `*?[\`) }

// Matches reports whether claims satisfy every condition. Claims that are
// not strings never match.
func (rule WorkloadRule) Matches(claims map[string]any) bool {
	for claim, pattern := range rule.Conditions {
		value, ok := claims[claim].(string)
		if !ok {
			return false
		}
		if matched, err := path.Match(pattern, value); err != nil || !matched {
			return false
		}
	}
	return true
}
