package queue

import (
	"context"
	"errors"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/diff"
	"github.com/daltoniam/overload/postgres"
)

// ChangedFiles lists the files a pull request changes in an enabled
// repository overload is configured for, using the repository's git host
// connection, GitHub App installation or the configured GitHub token. It is used to
// preview a workflow against a real pull request.
//
// repository is a repository's Ref: owner/name on GitHub or
// connection:path on a GitLab or Forgejo connection, so the same path on
// two hosts is never confused.
func ChangedFiles(ctx context.Context, store *postgres.Store, repository string, number int) ([]string, error) {
	repos, err := store.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	var selected *overload.Repository
	for _, repo := range repos {
		if repo.Ref() == repository && repo.Enabled {
			selected = &repo
			break
		}
	}
	if selected == nil {
		return nil, errors.New("repository is not configured and enabled in overload")
	}
	source, _, err := sourceFor(ctx, store, selected.ForgeID, selected.InstallationID)
	if err != nil {
		return nil, err
	}
	_, patch, err := source.PullRequestWithDiff(ctx, selected.FullName, number)
	if err != nil {
		return nil, errors.New("pull request not found or not readable")
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
