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
