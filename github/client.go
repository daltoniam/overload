package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/daltoniam/overload"
	gh "github.com/google/go-github/v75/github"
)

type Client struct {
	appID      int64
	privateKey []byte
	transport  *http.Transport
	api        *gh.Client
	token      string
	baseURL    string

	mu    sync.Mutex
	login string
}

func NewTokenClient(token string) (*Client, error) {
	if token == "" {
		return nil, errors.New("GitHub token required")
	}
	return &Client{api: gh.NewClient(&http.Client{Timeout: 30 * time.Second}).WithAuthToken(token), token: token}, nil
}

func NewClient(appID int64, key []byte) (*Client, error) {
	if appID < 1 || len(key) == 0 {
		return nil, errors.New("GitHub App ID and private key required")
	}
	return &Client{appID: appID, privateKey: key, transport: http.DefaultTransport.(*http.Transport).Clone()}, nil
}

func (client *Client) installation(id int64) (*gh.Client, error) {
	if client.api != nil {
		return client.api, nil
	}
	transport, err := ghinstallation.New(client.transport, client.appID, id, client.privateKey)
	if err != nil {
		return nil, err
	}
	api := gh.NewClient(&http.Client{Transport: transport, Timeout: 30 * time.Second})
	if client.baseURL != "" {
		transport.BaseURL = strings.TrimRight(client.baseURL, "/")
		parsed, err := url.Parse(transport.BaseURL + "/")
		if err != nil {
			return nil, err
		}
		api.BaseURL = parsed
	}
	return api, nil
}

func (client *Client) PullRequest(ctx context.Context, installationID int64, repository string, number int) (*gh.PullRequest, string, error) {
	owner, repo, err := splitRepository(repository)
	if err != nil {
		return nil, "", err
	}
	api, err := client.installation(installationID)
	if err != nil {
		return nil, "", err
	}
	pr, _, err := api.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, "", err
	}
	patch, _, err := api.PullRequests.GetRaw(ctx, owner, repo, number, gh.RawOptions{Type: gh.Diff})
	return pr, patch, err
}

func (client *Client) Tarball(ctx context.Context, installationID int64, repository, sha string, writer io.Writer) error {
	owner, repo, err := splitRepository(repository)
	if err != nil {
		return err
	}
	api, err := client.installation(installationID)
	if err != nil {
		return err
	}
	archive, _, err := api.Repositories.GetArchiveLink(ctx, owner, repo, gh.Tarball, &gh.RepositoryContentGetOptions{Ref: sha}, 3)
	if err != nil {
		return err
	}
	if err := validateArchiveURL(archive, strings.ToLower(api.BaseURL.Hostname())); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, archive.String(), nil)
	if err != nil {
		return err
	}
	return downloadArchive(ctx, request, writer)
}

func downloadArchive(ctx context.Context, request *http.Request, writer io.Writer) error {
	startHost := strings.ToLower(request.URL.Hostname())
	archiveClient := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many archive redirects")
		}
		return validateArchiveURL(req.URL, startHost)
	}}
	response, err := archiveClient.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			// Archive URLs carry short-lived access tokens; keep them out of logs.
			return fmt.Errorf("download archive: %w", urlErr.Err)
		}
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("archive HTTP %d", response.StatusCode)
	}
	written, err := io.Copy(writer, io.LimitReader(response.Body, (256<<20)+1))
	if err != nil {
		return err
	}
	if written > 256<<20 {
		return errors.New("archive too large")
	}
	return nil
}

// ReviewMarker tags a posted review with its run so retries can detect it.
func ReviewMarker(runID int64) string {
	return fmt.Sprintf("<!-- overload-run:%d -->", runID)
}

// author returns the login overload posts reviews as: the GitHub App's bot
// user, or the user a token belongs to.
func (client *Client) author(ctx context.Context) (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.login != "" {
		return client.login, nil
	}
	login, err := client.lookupAuthor(ctx)
	if err != nil {
		return "", err
	}
	if login == "" || login == "[bot]" {
		return "", errors.New("could not determine the GitHub account overload posts as")
	}
	client.login = login
	return login, nil
}

func (client *Client) lookupAuthor(ctx context.Context) (string, error) {
	if client.api != nil {
		user, _, err := client.api.Users.Get(ctx, "")
		return user.GetLogin(), err
	}
	api, err := client.appAPI()
	if err != nil {
		return "", err
	}
	app, _, err := api.Apps.Get(ctx, "")
	return app.GetSlug() + "[bot]", err
}

// appAPI is a client authenticated as the App itself (not an installation).
func (client *Client) appAPI() (*gh.Client, error) {
	if client.appID < 1 {
		return nil, errors.New("a GitHub App is required")
	}
	transport, err := ghinstallation.NewAppsTransport(client.transport, client.appID, client.privateKey)
	if err != nil {
		return nil, err
	}
	api := gh.NewClient(&http.Client{Transport: transport, Timeout: 30 * time.Second})
	if client.baseURL != "" {
		transport.BaseURL = strings.TrimRight(client.baseURL, "/")
		if api.BaseURL, err = url.Parse(transport.BaseURL + "/"); err != nil {
			return nil, err
		}
	}
	return api, nil
}

// SetWebhookURL points the App's webhook at url.
func (client *Client) SetWebhookURL(ctx context.Context, webhookURL string) error {
	api, err := client.appAPI()
	if err != nil {
		return err
	}
	_, _, err = api.Apps.UpdateHookConfig(ctx, &gh.HookConfig{URL: gh.Ptr(webhookURL), ContentType: gh.Ptr("json")})
	return err
}

// FindReview returns the ID of a review overload already posted on the PR
// whose body ends with marker, or 0. Reviews by anyone else are ignored, so
// a PR author cannot fake a marker to stop overload from posting.
func (client *Client) FindReview(ctx context.Context, installationID int64, repository string, number int, marker string) (int64, error) {
	owner, repo, err := splitRepository(repository)
	if err != nil {
		return 0, err
	}
	api, err := client.installation(installationID)
	if err != nil {
		return 0, err
	}
	login, err := client.author(ctx)
	if err != nil {
		return 0, err
	}
	options := &gh.ListOptions{PerPage: 100}
	for {
		reviews, response, err := api.PullRequests.ListReviews(ctx, owner, repo, number, options)
		if err != nil {
			return 0, err
		}
		for _, review := range reviews {
			if strings.EqualFold(review.GetUser().GetLogin(), login) && strings.HasSuffix(strings.TrimSpace(review.GetBody()), marker) {
				return review.GetID(), nil
			}
		}
		if response.NextPage == 0 {
			return 0, nil
		}
		options.Page = response.NextPage
	}
}

// ErrReviewRejected means GitHub refused the review itself, for example
// because the commit is no longer part of the pull request after a force
// push. Posting the same review again would fail the same way.
var ErrReviewRejected = errors.New("GitHub rejected the review")

func (client *Client) PostReview(ctx context.Context, installationID int64, repository string, number int, sha, summary string, findings []overload.Finding) (int64, error) {
	owner, repo, err := splitRepository(repository)
	if err != nil {
		return 0, err
	}
	api, err := client.installation(installationID)
	if err != nil {
		return 0, err
	}
	comments := make([]*gh.DraftReviewComment, 0, len(findings))
	for _, finding := range findings {
		body := "**" + finding.Title + "** (" + finding.Severity + ")\n\n" + finding.Body
		comments = append(comments, &gh.DraftReviewComment{Path: gh.Ptr(finding.Path), Line: gh.Ptr(finding.Line), Side: gh.Ptr("RIGHT"), Body: gh.Ptr(body)})
	}
	review, _, err := api.PullRequests.CreateReview(ctx, owner, repo, number, &gh.PullRequestReviewRequest{CommitID: gh.Ptr(sha), Body: gh.Ptr(summary), Event: gh.Ptr("COMMENT"), Comments: comments})
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil && response.Response.StatusCode == http.StatusUnprocessableEntity {
		return 0, fmt.Errorf("%w: %s", ErrReviewRejected, overload.TruncateUTF8(response.Message, 300))
	}
	if err != nil {
		return 0, err
	}
	return review.GetID(), nil
}

var archiveHosts = map[string]bool{"github.com": true, "api.github.com": true, "codeload.github.com": true}

// validateArchiveURL allows HTTPS archive URLs on GitHub's hosts or on the
// host the request started at (a GitHub Enterprise or test API).
func validateArchiveURL(archive *url.URL, startHost string) error {
	if archive == nil || archive.Scheme != "https" || archive.User != nil || archive.Hostname() == "" {
		return errors.New("invalid GitHub archive URL")
	}
	host := strings.ToLower(archive.Hostname())
	if !archiveHosts[host] && host != startHost {
		return fmt.Errorf("archive redirect to %s is not allowed", host)
	}
	return nil
}

func splitRepository(repository string) (string, string, error) {
	if !overload.ValidRepositoryName(repository) {
		return "", "", errors.New("repository must be owner/name")
	}
	owner, repo, _ := strings.Cut(repository, "/")
	return owner, repo, nil
}

func FromEnvironment() (*Client, error) {
	id, err := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
	if err != nil {
		return nil, err
	}
	key := []byte(os.Getenv("GITHUB_APP_PRIVATE_KEY"))
	if path := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"); path != "" {
		var err error
		key, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	return NewClient(id, key)
}
