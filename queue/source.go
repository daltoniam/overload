package queue

import (
	"context"
	"errors"

	"github.com/daltoniam/overload/diff"
	"github.com/daltoniam/overload/github"
	"github.com/daltoniam/overload/postgres"
)

// ChangedFiles lists the files a pull request changes in an enabled
// repository overload is configured for, using the repository's GitHub App
// installation or, without one, the configured GitHub token. It is used to
// preview a workflow against a real pull request.
func ChangedFiles(ctx context.Context, store *postgres.Store, repository string, number int) ([]string, error) {
	repos, err := store.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	var installationID int64 = -1
	for _, repo := range repos {
		if repo.FullName == repository && repo.Enabled {
			installationID = repo.InstallationID
		}
	}
	if installationID < 0 {
		return nil, errors.New("repository is not configured and enabled in overload")
	}
	var patch string
	if installationID > 0 {
		client, err := appClient(ctx, store)
		if err != nil {
			return nil, errors.New("GitHub App credentials unavailable")
		}
		_, patch, err = github.InstallationSource{Client: client, InstallationID: installationID}.PullRequestWithDiff(ctx, repository, number)
		if err != nil {
			return nil, errors.New("pull request not found or not readable")
		}
	} else {
		token := github.TokenFromEnvironment()
		if token == "" {
			return nil, errors.New("GitHub credentials unavailable")
		}
		client, err := github.NewTokenClient(token)
		if err != nil {
			return nil, errors.New("GitHub client unavailable")
		}
		if _, patch, err = client.PullRequestWithDiff(ctx, repository, number); err != nil {
			return nil, errors.New("pull request not found or not readable")
		}
	}
	files, err := diff.Parse(patch)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths, nil
}
