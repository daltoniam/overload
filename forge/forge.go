// Package forge reads pull requests from and posts reviews to GitLab and
// Forgejo (or Gitea) servers with an access token.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/daltoniam/overload"
)

// Client is what overload needs from a git host: reading a pull request,
// its diff and head, posting a review and setting a commit status. The
// installation-free signatures match the GitHub token client.
type Client interface {
	PullRequestWithDiff(ctx context.Context, repository string, number int) (overload.PullRequest, string, error)
	DownloadHead(ctx context.Context, repository, sha string, writer io.Writer) error
	FindReview(ctx context.Context, repository string, number int, marker string) (int64, error)
	PostReview(ctx context.Context, repository string, number int, sha, event, summary string, findings []overload.Finding) (int64, error)
	ChangesRequested(ctx context.Context, repository string, number int) ([]int64, error)
	DismissReview(ctx context.Context, repository string, number int, reviewID int64, message string) error
	SetStatus(ctx context.Context, repository, sha, state, description, targetURL string) error
	// CreateWebhook adds (or updates) the repository webhook that sends
	// pull request events to url.
	CreateWebhook(ctx context.Context, repository, url, secret string) error
	// Identity is the account the token belongs to, to check a token.
	Identity(ctx context.Context) (string, error)
}

// StatusContext is the name overload's commit status appears under.
const StatusContext = "overload"

// Limits on what a host may send back.
const (
	maxJSONBytes    = 16 << 20
	maxDiffBytes    = 4 << 20
	maxArchiveBytes = 256 << 20
)

// maxPages bounds how many pages of a list are read. A list longer than
// that is an error, never treated as complete: a missed review or hook
// would post twice or leave a request for changes behind.
const maxPages = 50

// errTooManyPages is returned when a list does not end within maxPages.
var errTooManyPages = fmt.Errorf("list has more than %d pages", maxPages)

var commitSHA = regexp.MustCompile(`^[a-fA-F0-9]{40}([a-fA-F0-9]{24})?$`)

// New returns the client for a connection.
func New(connection overload.ForgeConnection) (Client, error) {
	base, err := overload.ForgeBaseURL(connection.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(connection.Token) == "" {
		return nil, errors.New("access token required")
	}
	switch connection.Kind {
	case overload.ForgeGitLab:
		return &GitLab{api: newAPI(base+"/api/v4", func(request *http.Request) { request.Header.Set("PRIVATE-TOKEN", connection.Token) })}, nil
	case overload.ForgeForgejo:
		return &Forgejo{api: newAPI(base+"/api/v1", func(request *http.Request) { request.Header.Set("Authorization", "token "+connection.Token) })}, nil
	}
	return nil, fmt.Errorf("unsupported git host kind %q", connection.Kind)
}

// apiError is a non-2xx answer from a host's API.
type apiError struct {
	Status  int
	Message string
}

func (err *apiError) Error() string {
	if err.Message == "" {
		return fmt.Sprintf("HTTP %d", err.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", err.Status, err.Message)
}

func statusOf(err error) int {
	var response *apiError
	if errors.As(err, &response) {
		return response.Status
	}
	return 0
}

type api struct {
	base    string
	auth    func(*http.Request)
	client  *http.Client
	archive *http.Client
}

func newAPI(base string, auth func(*http.Request)) *api {
	// Redirects stay on the host: Go would forward custom token headers
	// such as PRIVATE-TOKEN to any host a redirect names.
	sameHost := func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if request.URL.Host != via[0].URL.Host || (via[0].URL.Scheme == "https" && request.URL.Scheme != "https") {
			return fmt.Errorf("redirect to %s is not allowed", request.URL.Host)
		}
		return nil
	}
	return &api{
		base:    base,
		auth:    auth,
		client:  &http.Client{Timeout: time.Minute, CheckRedirect: sameHost},
		archive: &http.Client{Timeout: 10 * time.Minute, CheckRedirect: sameHost},
	}
}

func (a *api) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "overload")
	a.auth(request)
	return request, nil
}

// do sends a JSON request and decodes a JSON answer into out (when not
// nil). It returns the response headers for pagination.
func (a *api) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	request, err := a.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxJSONBytes {
		return nil, errors.New("response too large")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return response.Header, &apiError{Status: response.StatusCode, Message: errorMessage(data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return response.Header, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return response.Header, nil
}

// text fetches a plain text body of at most limit bytes.
func (a *api) text(ctx context.Context, path string, limit int64) (string, error) {
	request, err := a.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "text/plain")
	response, err := a.client.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", &apiError{Status: response.StatusCode, Message: errorMessage(data)}
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("diff exceeds %d MiB", limit>>20)
	}
	return string(data), nil
}

// download streams an archive to writer.
func (a *api) download(ctx context.Context, path string, writer io.Writer) error {
	request, err := a.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/octet-stream")
	response, err := a.archive.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &apiError{Status: response.StatusCode, Message: errorMessage(data)}
	}
	written, err := io.Copy(writer, io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return err
	}
	if written > maxArchiveBytes {
		return errors.New("archive exceeds 256 MiB")
	}
	return nil
}

// errorMessage pulls a short message out of an error body without
// echoing large or binary answers.
func errorMessage(data []byte) string {
	var parsed struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(data, &parsed) == nil {
		switch message := parsed.Message.(type) {
		case string:
			return overload.TruncateUTF8(message, 300)
		case nil:
		default:
			if encoded, err := json.Marshal(message); err == nil {
				return overload.TruncateUTF8(string(encoded), 300)
			}
		}
		if parsed.Error != "" {
			return overload.TruncateUTF8(parsed.Error, 300)
		}
	}
	return ""
}

// commentBody formats a finding the same way on every host.
func commentBody(finding overload.Finding) string {
	return "**" + finding.Title + "** (" + finding.Severity + ")\n\n" + finding.Body
}
