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
// it trusts: GitHub rules must name a repository or owner, since every
// GitHub Actions job can get a token from the same issuer.
func (rule WorkloadRule) Validate() error {
	if !settingName.MatchString(rule.Name) {
		return errors.New("invalid rule name")
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
	if rule.Issuer == GitHubActionsIssuer && rule.Conditions["repository"] == "" && rule.Conditions["repository_owner"] == "" {
		return errors.New("GitHub Actions rules must match repository or repository_owner")
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
