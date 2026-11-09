package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/daltoniam/overload"
	gh "github.com/google/go-github/v75/github"
)

// maxRepliesPerFinding bounds the replies a re-review is shown per finding.
const maxRepliesPerFinding = 3

// commentTitle is how a posted finding's comment starts; see PostReview.
func commentTitle(title string) string {
	return "**" + title + "**"
}

// PriorReplies returns, for each earlier finding, the replies people left
// on overload's review comment for it, oldest first. Overload's own
// comments are matched by author, path and title; replies by overload are
// left out.
func (client *Client) PriorReplies(ctx context.Context, installationID int64, repository string, number int, prior []overload.PriorFinding) (map[int64][]string, error) {
	owner, repo, err := splitRepository(repository)
	if err != nil {
		return nil, err
	}
	api, err := client.installation(installationID)
	if err != nil {
		return nil, err
	}
	login, err := client.author(ctx)
	if err != nil {
		return nil, err
	}
	var comments []*gh.PullRequestComment
	options := &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; page < 20; page++ {
		batch, response, err := api.PullRequests.ListComments(ctx, owner, repo, number, options)
		if err != nil {
			return nil, err
		}
		comments = append(comments, batch...)
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	findingOf := map[int64]int64{}
	for _, comment := range comments {
		if !strings.EqualFold(comment.GetUser().GetLogin(), login) || comment.GetInReplyTo() != 0 {
			continue
		}
		for _, finding := range prior {
			if comment.GetPath() == finding.Path && strings.HasPrefix(comment.GetBody(), commentTitle(finding.Title)) {
				findingOf[comment.GetID()] = finding.ID
			}
		}
	}
	replies := map[int64][]string{}
	for _, comment := range comments {
		finding, ok := findingOf[comment.GetInReplyTo()]
		if !ok || strings.EqualFold(comment.GetUser().GetLogin(), login) || len(replies[finding]) >= maxRepliesPerFinding {
			continue
		}
		replies[finding] = append(replies[finding], comment.GetUser().GetLogin()+": "+comment.GetBody())
	}
	return replies, nil
}

// PriorReplies reads replies through the installation.
func (source InstallationSource) PriorReplies(ctx context.Context, repository string, number int, prior []overload.PriorFinding) (map[int64][]string, error) {
	if source.InstallationID < 1 {
		return nil, errors.New("installation ID required")
	}
	return source.Client.PriorReplies(ctx, source.InstallationID, repository, number, prior)
}

const reviewThreadsQuery = `query($owner: String!, $repo: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { id isResolved comments(first: 1) { nodes { path body author { login } } } }
      }
    }
  }
}`

const resolveThreadMutation = `mutation($thread: ID!) { resolveReviewThread(input: {threadId: $thread}) { thread { id } } }`

type graphQLError struct {
	Message string `json:"message"`
}

// graphQL runs one GitHub GraphQL request through the installation.
func (client *Client) graphQL(ctx context.Context, installationID int64, query string, variables map[string]any, out any) error {
	api, err := client.installation(installationID)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSuffix(api.BaseURL.String(), "/")
	if strings.HasSuffix(endpoint, "/api/v3") {
		endpoint = strings.TrimSuffix(endpoint, "/v3") + "/graphql"
	} else {
		endpoint += "/graphql"
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := api.Client().Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub GraphQL: HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("GitHub GraphQL: %s", overload.TruncateUTF8(envelope.Errors[0].Message, 200))
	}
	return json.Unmarshal(envelope.Data, out)
}

// ResolveThreads resolves overload's open review threads for findings a
// re-review found fixed, and returns how many it resolved. Threads are
// matched by author, path and title, like PriorReplies.
func (client *Client) ResolveThreads(ctx context.Context, installationID int64, repository string, number int, fixed []overload.Finding) (int, error) {
	if len(fixed) == 0 {
		return 0, nil
	}
	owner, repo, err := splitRepository(repository)
	if err != nil {
		return 0, err
	}
	login, err := client.author(ctx)
	if err != nil {
		return 0, err
	}
	var threads []string
	var after *string
	for page := 0; page < 10; page++ {
		var data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									Path   string `json:"path"`
									Body   string `json:"body"`
									Author struct {
										Login string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := client.graphQL(ctx, installationID, reviewThreadsQuery, map[string]any{"owner": owner, "repo": repo, "number": number, "after": after}, &data); err != nil {
			return 0, err
		}
		page := data.Repository.PullRequest.ReviewThreads
		for _, thread := range page.Nodes {
			if thread.IsResolved || len(thread.Comments.Nodes) == 0 {
				continue
			}
			first := thread.Comments.Nodes[0]
			// GraphQL reports the App's bot without the [bot] suffix.
			if !strings.EqualFold(first.Author.Login, strings.TrimSuffix(login, "[bot]")) && !strings.EqualFold(first.Author.Login, login) {
				continue
			}
			for _, finding := range fixed {
				if first.Path == finding.Path && strings.HasPrefix(first.Body, commentTitle(finding.Title)) {
					threads = append(threads, thread.ID)
					break
				}
			}
		}
		if !page.PageInfo.HasNextPage {
			break
		}
		cursor := page.PageInfo.EndCursor
		after = &cursor
	}
	resolved := 0
	for _, thread := range threads {
		var out json.RawMessage
		if err := client.graphQL(ctx, installationID, resolveThreadMutation, map[string]any{"thread": thread}, &out); err != nil {
			return resolved, err
		}
		resolved++
	}
	return resolved, nil
}
