package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/forge"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
	reviewpkg "github.com/daltoniam/overload/review"
)

// ForgeClient returns the client for a GitLab or Forgejo connection.
func ForgeClient(ctx context.Context, store *postgres.Store, forgeID int64) (forge.Client, error) {
	connection, err := store.LoadForge(ctx, forgeID)
	if err != nil {
		return nil, err
	}
	return forge.New(connection)
}

// forgePoster adapts a forge client to the GitHub-shaped ReviewPoster and
// StatusSetter, which carry an installation ID forges do not use.
type forgePoster struct {
	client forge.Client
}

func (poster forgePoster) FindReview(ctx context.Context, _ int64, repository string, number int, marker string) (int64, error) {
	return poster.client.FindReview(ctx, repository, number, marker)
}

func (poster forgePoster) PostReview(ctx context.Context, _ int64, repository string, number int, sha, event, summary string, findings []overload.Finding) (int64, error) {
	return poster.client.PostReview(ctx, repository, number, sha, event, summary, findings)
}

func (poster forgePoster) ChangesRequested(ctx context.Context, _ int64, repository string, number int) ([]int64, error) {
	return poster.client.ChangesRequested(ctx, repository, number)
}

func (poster forgePoster) DismissReview(ctx context.Context, _ int64, repository string, number int, reviewID int64, message string) error {
	return poster.client.DismissReview(ctx, repository, number, reviewID, message)
}

func (poster forgePoster) SetStatus(ctx context.Context, _ int64, repository, sha, state, description, targetURL string) error {
	return poster.client.SetStatus(ctx, repository, sha, state, description, targetURL)
}

// PullRequestReader reads a pull request's current state from the host
// the repository is on.
type PullRequestReader interface {
	PullRequest(ctx context.Context, repository overload.Repository, number int) (overload.PullRequest, error)
}

// HostReader reads pull requests through the GitHub App installation or
// the GitLab or Forgejo connection a repository is linked to.
func HostReader(store *postgres.Store) PullRequestReader {
	return hostReader{store: store}
}

type hostReader struct {
	store *postgres.Store
}

func (reader hostReader) PullRequest(ctx context.Context, repository overload.Repository, number int) (overload.PullRequest, error) {
	if repository.ForgeID > 0 {
		client, err := ForgeClient(ctx, reader.store, repository.ForgeID)
		if err != nil {
			return overload.PullRequest{}, fmt.Errorf("git host connection: %w", err)
		}
		pr, _, err := client.PullRequestWithDiff(ctx, repository.FullName, number)
		return pr, err
	}
	if repository.InstallationID == 0 {
		return overload.PullRequest{}, errors.New("repository is not linked to the GitHub App")
	}
	client, err := appClient(ctx, reader.store)
	if err != nil {
		return overload.PullRequest{}, errors.New("GitHub App credentials unavailable")
	}
	pr, _, err := client.PullRequest(ctx, repository.InstallationID, repository.FullName, number)
	return pr, err
}

// sourceFor returns where a run's pull request and head archive come
// from: its forge connection, its GitHub App installation, or the
// configured GitHub token. The code names the missing credential.
func sourceFor(ctx context.Context, store *postgres.Store, forgeID, installationID int64) (reviewpkg.PullRequestSource, string, error) {
	if forgeID > 0 {
		client, err := ForgeClient(ctx, store, forgeID)
		if err != nil {
			return nil, "missing_forge_connection", fmt.Errorf("git host connection unavailable: %w", err)
		}
		return client, "", nil
	}
	if installationID > 0 {
		client, err := appClient(ctx, store)
		if err != nil {
			return nil, "missing_github_app", errors.New("GitHub App credentials unavailable")
		}
		return github.InstallationSource{Client: client, InstallationID: installationID}, "", nil
	}
	token := github.TokenFromEnvironment()
	if token == "" {
		return nil, "missing_github_credentials", errors.New("GitHub credentials unavailable")
	}
	client, err := github.NewTokenClient(token)
	if err != nil {
		return nil, "github_client", errors.New("GitHub client unavailable")
	}
	return client, "", nil
}
