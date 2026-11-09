package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/daltoniam/overload"
)

// Forgejo talks to the Forgejo (and Gitea) REST API (v1) with an access
// token that can read and write the repository and its issues.
type Forgejo struct {
	api   *api
	mu    sync.Mutex
	login string
}

type forgejoUser struct {
	Login string `json:"login"`
}

func forgejoRepository(repository string) (string, error) {
	if !overload.ValidForgeRepositoryName(overload.ForgeForgejo, repository) {
		return "", errors.New("repository must be owner/name")
	}
	owner, name, _ := strings.Cut(repository, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

func (forgejo *Forgejo) Identity(ctx context.Context) (string, error) {
	forgejo.mu.Lock()
	defer forgejo.mu.Unlock()
	if forgejo.login != "" {
		return forgejo.login, nil
	}
	var user forgejoUser
	if _, err := forgejo.api.do(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", errors.New("forgejo did not return the token's user")
	}
	forgejo.login = user.Login
	return user.Login, nil
}

type forgejoPullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Draft  bool   `json:"draft"`
	Merged bool   `json:"merged"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA  string `json:"sha"`
		Repo struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"base"`
}

// workInProgress reports a title Forgejo treats as a draft. Older
// servers have no draft field and only use these prefixes.
func workInProgress(title string) bool {
	lower := strings.ToLower(strings.TrimSpace(title))
	return strings.HasPrefix(lower, "wip:") || strings.HasPrefix(lower, "[wip]") || strings.HasPrefix(lower, "draft:") || strings.HasPrefix(lower, "[draft]")
}

func (forgejo *Forgejo) PullRequestWithDiff(ctx context.Context, repository string, number int) (overload.PullRequest, string, error) {
	repo, err := forgejoRepository(repository)
	if err != nil {
		return overload.PullRequest{}, "", err
	}
	if number < 1 {
		return overload.PullRequest{}, "", errors.New("pull request number must be positive")
	}
	path := repo + "/pulls/" + strconv.Itoa(number)
	var raw forgejoPullRequest
	if _, err := forgejo.api.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return overload.PullRequest{}, "", err
	}
	state := raw.State
	if raw.Merged {
		state = "merged"
	}
	pr := overload.PullRequest{Number: raw.Number, State: state, Draft: raw.Draft || workInProgress(raw.Title), HeadSHA: raw.Head.SHA, BaseSHA: raw.Base.SHA, RepositoryID: raw.Base.Repo.ID}
	patch, err := forgejo.api.text(ctx, path+".diff", maxDiffBytes)
	return pr, patch, err
}

func (forgejo *Forgejo) DownloadHead(ctx context.Context, repository, sha string, writer io.Writer) error {
	repo, err := forgejoRepository(repository)
	if err != nil {
		return err
	}
	if !commitSHA.MatchString(sha) {
		return errors.New("invalid head SHA")
	}
	return forgejo.api.download(ctx, repo+"/archive/"+sha+".tar.gz", writer)
}

type forgejoReview struct {
	ID        int64       `json:"id"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Dismissed bool        `json:"dismissed"`
	User      forgejoUser `json:"user"`
}

func (forgejo *Forgejo) ownReviews(ctx context.Context, repository string, number int) ([]forgejoReview, error) {
	repo, err := forgejoRepository(repository)
	if err != nil {
		return nil, err
	}
	login, err := forgejo.Identity(ctx)
	if err != nil {
		return nil, err
	}
	var own []forgejoReview
	for page := 1; page <= 20; page++ {
		var reviews []forgejoReview
		if _, err := forgejo.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d/reviews?limit=50&page=%d", repo, number, page), nil, &reviews); err != nil {
			return nil, err
		}
		for _, review := range reviews {
			if strings.EqualFold(review.User.Login, login) {
				own = append(own, review)
			}
		}
		if len(reviews) < 50 {
			break
		}
	}
	return own, nil
}

func (forgejo *Forgejo) FindReview(ctx context.Context, repository string, number int, marker string) (int64, error) {
	reviews, err := forgejo.ownReviews(ctx, repository, number)
	if err != nil {
		return 0, err
	}
	for _, review := range reviews {
		if strings.Contains(review.Body, marker) {
			return review.ID, nil
		}
	}
	return 0, nil
}

// forgejoEvents maps overload's review events to Forgejo's.
var forgejoEvents = map[string]string{overload.ReviewComment: "COMMENT", overload.ReviewRequestChanges: "REQUEST_CHANGES", overload.ReviewApprove: "APPROVED"}

func (forgejo *Forgejo) PostReview(ctx context.Context, repository string, number int, sha, event, summary string, findings []overload.Finding) (int64, error) {
	repo, err := forgejoRepository(repository)
	if err != nil {
		return 0, err
	}
	mapped, ok := forgejoEvents[event]
	if !ok {
		return 0, fmt.Errorf("invalid review event %q", event)
	}
	comments := make([]map[string]any, 0, len(findings))
	for _, finding := range findings {
		comments = append(comments, map[string]any{"path": finding.Path, "body": commentBody(finding), "new_position": finding.Line})
	}
	var review forgejoReview
	_, err = forgejo.api.do(ctx, http.MethodPost, fmt.Sprintf("%s/pulls/%d/reviews", repo, number), map[string]any{"commit_id": sha, "body": summary, "event": mapped, "comments": comments}, &review)
	if status := statusOf(err); status == http.StatusUnprocessableEntity || status == http.StatusNotFound || status == http.StatusBadRequest {
		return 0, fmt.Errorf("%w: %v", overload.ErrReviewRejected, err)
	}
	if err != nil {
		return 0, err
	}
	return review.ID, nil
}

func (forgejo *Forgejo) ChangesRequested(ctx context.Context, repository string, number int) ([]int64, error) {
	reviews, err := forgejo.ownReviews(ctx, repository, number)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, review := range reviews {
		if review.State == "REQUEST_CHANGES" && !review.Dismissed {
			ids = append(ids, review.ID)
		}
	}
	return ids, nil
}

// DismissReview withdraws an earlier request for changes. Dismissing
// needs repository admin rights on Forgejo; without them the request is
// left in place and a warning logged rather than retrying forever.
func (forgejo *Forgejo) DismissReview(ctx context.Context, repository string, number int, reviewID int64, message string) error {
	repo, err := forgejoRepository(repository)
	if err != nil {
		return err
	}
	_, err = forgejo.api.do(ctx, http.MethodPost, fmt.Sprintf("%s/pulls/%d/reviews/%d/dismissals", repo, number, reviewID), map[string]string{"message": message}, nil)
	if status := statusOf(err); status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		slog.Warn("forgejo review not dismissed; the token needs repository admin rights", "repository", repository, "pull_request", number, "error", err)
		return nil
	}
	return err
}

var forgejoStates = map[string]bool{"pending": true, "success": true, "failure": true, "error": true}

func (forgejo *Forgejo) SetStatus(ctx context.Context, repository, sha, state, description, targetURL string) error {
	repo, err := forgejoRepository(repository)
	if err != nil {
		return err
	}
	if !commitSHA.MatchString(sha) {
		return errors.New("invalid commit SHA")
	}
	if !forgejoStates[state] {
		return fmt.Errorf("invalid status state %q", state)
	}
	body := map[string]string{"state": state, "context": StatusContext, "description": overload.TruncateUTF8(description, 255)}
	if targetURL != "" {
		body["target_url"] = targetURL
	}
	_, err = forgejo.api.do(ctx, http.MethodPost, repo+"/statuses/"+sha, body, nil)
	if status := statusOf(err); status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusUnauthorized {
		return fmt.Errorf("%w: %v", overload.ErrStatusForbidden, err)
	}
	return err
}

type forgejoHook struct {
	ID     int64             `json:"id"`
	Config map[string]string `json:"config"`
}

// CreateWebhook adds a Gitea-style webhook for pull request events. The
// "gitea" type works on both Forgejo and Gitea and signs deliveries with
// the secret.
func (forgejo *Forgejo) CreateWebhook(ctx context.Context, repository, hookURL, secret string) error {
	repo, err := forgejoRepository(repository)
	if err != nil {
		return err
	}
	config := map[string]string{"url": hookURL, "content_type": "json", "secret": secret}
	events := []string{"pull_request", "pull_request_sync"}
	var hooks []forgejoHook
	if _, err := forgejo.api.do(ctx, http.MethodGet, repo+"/hooks?limit=50", nil, &hooks); err != nil {
		return err
	}
	for _, hook := range hooks {
		if hook.Config["url"] == hookURL {
			_, err := forgejo.api.do(ctx, http.MethodPatch, fmt.Sprintf("%s/hooks/%d", repo, hook.ID), map[string]any{"config": config, "events": events, "active": true}, nil)
			return err
		}
	}
	_, err = forgejo.api.do(ctx, http.MethodPost, repo+"/hooks", map[string]any{"type": "gitea", "config": config, "events": events, "active": true}, nil)
	return err
}
