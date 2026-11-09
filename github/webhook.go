package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type PullRequestEvent struct {
	Action       string `json:"action"`
	Number       int    `json:"number"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Draft bool `json:"draft"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
		User struct {
			Type string `json:"type"`
		} `json:"user"`
	} `json:"pull_request"`
}

func Verify(body []byte, signature, secret string) bool {
	if secret == "" || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

func ParsePullRequest(body []byte) (PullRequestEvent, error) {
	var event PullRequestEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return event, err
	}
	if event.Repository.FullName == "" || event.Number < 1 {
		return event, errors.New("missing repository or PR number")
	}
	return event, nil
}

func ShouldReview(event PullRequestEvent) bool {
	if event.PullRequest.Draft || event.PullRequest.User.Type == "Bot" || strings.TrimSpace(event.PullRequest.Head.SHA) == "" || strings.TrimSpace(event.PullRequest.Base.SHA) == "" {
		return false
	}
	switch event.Action {
	case "opened", "synchronize", "reopened", "ready_for_review":
		return true
	default:
		return false
	}
}

// ReviewCommand is the comment that asks overload to review a pull
// request again, regardless of its re-review limit.
const ReviewCommand = "/overload review"

// CommentEvent is an issue_comment webhook, reduced to what the review
// command needs.
type CommentEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		Body              string `json:"body"`
		AuthorAssociation string `json:"author_association"`
		User              struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"comment"`
}

// ParseComment reads an issue_comment webhook.
func ParseComment(body []byte) (CommentEvent, error) {
	var event CommentEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return event, err
	}
	if event.Repository.FullName == "" || event.Issue.Number < 1 {
		return event, errors.New("comment event missing repository or number")
	}
	return event, nil
}

// RequestsReview reports whether the comment asks for a review: a new
// comment on a pull request whose first line is the review command, from
// someone with write access (owner, member or collaborator), not a bot.
func (event CommentEvent) RequestsReview() bool {
	if event.Action != "created" || len(event.Issue.PullRequest) == 0 || string(event.Issue.PullRequest) == "null" || event.Comment.User.Type == "Bot" {
		return false
	}
	switch event.Comment.AuthorAssociation {
	case "OWNER", "MEMBER", "COLLABORATOR":
	default:
		return false
	}
	first, _, _ := strings.Cut(strings.TrimSpace(event.Comment.Body), "\n")
	return strings.EqualFold(strings.TrimSpace(first), ReviewCommand)
}
