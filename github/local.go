package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v75/github"
)

var commitSHA = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

func (client *Client) PullRequestWithDiff(ctx context.Context, repository string, number int) (*gh.PullRequest, string, error) {
	return client.PullRequest(ctx, 0, repository, number)
}

func (client *Client) DownloadHead(ctx context.Context, repository, sha string, writer io.Writer) error {
	if client.token == "" {
		return client.Tarball(ctx, 0, repository, sha, writer)
	}
	owner, repo, err := splitRepository(repository)
	if err != nil {
		return err
	}
	if !commitSHA.MatchString(sha) {
		return errors.New("invalid head SHA")
	}
	endpoint := client.api.BaseURL.ResolveReference(&url.URL{Path: fmt.Sprintf("repos/%s/%s/tarball/%s", owner, repo, sha)})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	return downloadArchive(ctx, request, writer)
}

func TokenFromGH(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("obtain GitHub token from gh: %w", err)
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", errors.New("gh returned an empty GitHub token")
	}
	return token, nil
}

// TokenFromEnvironment returns GITHUB_TOKEN, or GH_TOKEN as the gh CLI names it.
func TokenFromEnvironment() string {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token
		}
	}
	return ""
}
