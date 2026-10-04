package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v75/github"
)

// AppCredentials are the secrets GitHub returns once when an App is created
// from a manifest.
type AppCredentials struct {
	AppID         int64
	Slug          string
	HTMLURL       string
	PrivateKey    string
	WebhookSecret string
}

type manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     map[string]any    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	SetupURL           string            `json:"setup_url,omitempty"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// Manifest returns the GitHub App manifest JSON for an overload install.
// baseURL is where the browser reaches overload (for the redirect after
// creation); webhookURL must be reachable by GitHub.
func Manifest(name, baseURL, webhookURL string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return "", errors.New("invalid overload base URL")
	}
	hook, err := url.Parse(webhookURL)
	if err != nil || hook.Scheme != "https" || hook.Host == "" || hook.User != nil {
		return "", errors.New("webhook URL must be an https URL reachable by GitHub")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 34 {
		return "", errors.New("app name must be 1 to 34 characters")
	}
	root := strings.TrimRight(base.String(), "/")
	data, err := json.Marshal(manifest{
		Name:           name,
		URL:            "https://github.com/daltoniam/overload",
		HookAttributes: map[string]any{"url": hook.String(), "active": true},
		RedirectURL:    root + "/setup/github/callback",
		SetupURL:       root + "/configure/repositories",
		Public:         false,
		DefaultPermissions: map[string]string{
			"contents":      "read",
			"metadata":      "read",
			"pull_requests": "write",
		},
		DefaultEvents: []string{"pull_request"},
	})
	return string(data), err
}

// ManifestFormAction is the GitHub page that receives the manifest form.
func ManifestFormAction(organization string) (string, error) {
	organization = strings.TrimSpace(organization)
	if organization == "" {
		return "https://github.com/settings/apps/new", nil
	}
	for _, r := range organization {
		if r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return "", errors.New("invalid organization name")
		}
	}
	return "https://github.com/organizations/" + organization + "/settings/apps/new", nil
}

// CompleteManifest exchanges the one-time code from the manifest redirect for
// the new App's credentials. apiBase overrides the GitHub API for tests.
func CompleteManifest(ctx context.Context, code string, apiBase string) (AppCredentials, error) {
	if code == "" || strings.ContainsAny(code, "/?#\n") {
		return AppCredentials{}, errors.New("invalid manifest code")
	}
	api := gh.NewClient(&http.Client{Timeout: 30 * time.Second})
	if apiBase != "" {
		parsed, err := url.Parse(strings.TrimRight(apiBase, "/") + "/")
		if err != nil {
			return AppCredentials{}, err
		}
		api.BaseURL = parsed
	}
	config, _, err := api.Apps.CompleteAppManifest(ctx, code)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("complete app manifest: %w", err)
	}
	credentials := AppCredentials{AppID: config.GetID(), Slug: config.GetSlug(), HTMLURL: config.GetHTMLURL(), PrivateKey: config.GetPEM(), WebhookSecret: config.GetWebhookSecret()}
	if credentials.AppID < 1 || credentials.PrivateKey == "" || credentials.WebhookSecret == "" {
		return AppCredentials{}, errors.New("GitHub returned incomplete app credentials")
	}
	return credentials, nil
}

type InstallationRepository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

// InstallationEvent covers the installation and installation_repositories
// webhooks.
type InstallationEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	} `json:"installation"`
	Repositories        []InstallationRepository `json:"repositories"`
	RepositoriesAdded   []InstallationRepository `json:"repositories_added"`
	RepositoriesRemoved []InstallationRepository `json:"repositories_removed"`
}

func ParseInstallation(body []byte) (InstallationEvent, error) {
	var event InstallationEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return event, err
	}
	if event.Installation.ID < 1 || event.Action == "" {
		return event, errors.New("missing installation or action")
	}
	if event.Installation.Account.Login == "" {
		event.Installation.Account.Login = "unknown"
	}
	if event.Installation.Account.Type == "" {
		event.Installation.Account.Type = "unknown"
	}
	for _, list := range [][]InstallationRepository{event.Repositories, event.RepositoriesAdded, event.RepositoriesRemoved} {
		for _, repo := range list {
			if repo.ID < 1 {
				return event, errors.New("repository missing id")
			}
			if _, _, err := splitRepository(repo.FullName); err != nil {
				return event, err
			}
		}
	}
	return event, nil
}

// InstallationSource fetches PR data with an installation token.
type InstallationSource struct {
	Client         *Client
	InstallationID int64
}

func (source InstallationSource) PullRequestWithDiff(ctx context.Context, repository string, number int) (*gh.PullRequest, string, error) {
	if source.InstallationID < 1 {
		return nil, "", errors.New("installation ID required")
	}
	return source.Client.PullRequest(ctx, source.InstallationID, repository, number)
}

func (source InstallationSource) DownloadHead(ctx context.Context, repository, sha string, writer io.Writer) error {
	if source.InstallationID < 1 {
		return errors.New("installation ID required")
	}
	if !commitSHA.MatchString(sha) {
		return errors.New("invalid head SHA")
	}
	return source.Client.Tarball(ctx, source.InstallationID, repository, sha, writer)
}
