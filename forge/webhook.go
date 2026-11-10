package forge

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/daltoniam/overload"
)

// Event is a pull request webhook from GitLab or Forgejo with its action
// translated to the GitHub names trigger bindings use (opened,
// synchronize, reopened, ready_for_review).
type Event struct {
	Action       string
	Repository   string
	RepositoryID int64
	Number       int
	HeadSHA      string
	BaseSHA      string
	// Eligible is set when the event should start a review: an open, non
	// draft pull request that was opened, reopened, marked ready or got
	// new commits.
	Eligible bool
}

// ErrNotPullRequest is returned for deliveries that are not pull request
// events (pushes, test pings); they are acknowledged and ignored.
var ErrNotPullRequest = errors.New("not a pull request event")

// Delivery returns a webhook's delivery ID and event name from its
// headers.
func Delivery(kind string, header http.Header) (string, string) {
	switch kind {
	case overload.ForgeGitLab:
		id := header.Get("X-Gitlab-Event-UUID")
		if id == "" {
			id = header.Get("X-Gitlab-Webhook-UUID") + "-" + header.Get("Idempotency-Key")
		}
		return strings.Trim(id, "-"), header.Get("X-Gitlab-Event")
	case overload.ForgeForgejo:
		id, event := header.Get("X-Forgejo-Delivery"), header.Get("X-Forgejo-Event")
		if id == "" {
			id = header.Get("X-Gitea-Delivery")
		}
		if event == "" {
			event = header.Get("X-Gitea-Event")
		}
		return id, event
	}
	return "", ""
}

// Verify checks a webhook against the connection's secret: GitLab sends
// the secret itself in X-Gitlab-Token, Forgejo signs the body with
// HMAC-SHA256 in X-Forgejo-Signature (X-Gitea-Signature on Gitea).
func Verify(kind string, header http.Header, body []byte, secret string) bool {
	if secret == "" {
		return false
	}
	switch kind {
	case overload.ForgeGitLab:
		token := header.Get("X-Gitlab-Token")
		return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
	case overload.ForgeForgejo:
		signature := header.Get("X-Forgejo-Signature")
		if signature == "" {
			signature = header.Get("X-Gitea-Signature")
		}
		given, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
		if err != nil || len(given) != sha256.Size {
			return false
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		return hmac.Equal(given, mac.Sum(nil))
	}
	return false
}

// Parse reads a pull request webhook body. It returns ErrNotPullRequest
// for other events.
func Parse(kind, event string, body []byte) (Event, error) {
	switch kind {
	case overload.ForgeGitLab:
		if event != "Merge Request Hook" {
			return Event{}, ErrNotPullRequest
		}
		return parseGitLab(body)
	case overload.ForgeForgejo:
		if event != "pull_request" {
			return Event{}, ErrNotPullRequest
		}
		return parseForgejo(body)
	}
	return Event{}, errors.New("unsupported git host kind")
}

type gitlabHookPayload struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		IID            int    `json:"iid"`
		Action         string `json:"action"`
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		WorkInProgress bool   `json:"work_in_progress"`
		OldRev         string `json:"oldrev"`
		LastCommit     struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
	Changes struct {
		Draft *struct {
			Previous bool `json:"previous"`
			Current  bool `json:"current"`
		} `json:"draft"`
	} `json:"changes"`
}

func parseGitLab(body []byte) (Event, error) {
	var payload gitlabHookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return Event{}, err
	}
	if payload.ObjectKind != "merge_request" {
		return Event{}, ErrNotPullRequest
	}
	attributes := payload.ObjectAttributes
	event := Event{
		Action:       attributes.Action,
		Repository:   payload.Project.PathWithNamespace,
		RepositoryID: payload.Project.ID,
		Number:       attributes.IID,
		HeadSHA:      attributes.LastCommit.ID,
	}
	if event.Repository == "" || event.Number < 1 {
		return Event{}, errors.New("merge request event missing project or IID")
	}
	switch {
	case attributes.Action == "open":
		event.Action = "opened"
	case attributes.Action == "reopen":
		event.Action = "reopened"
	case attributes.Action == "update" && attributes.OldRev != "":
		event.Action = "synchronize"
	case attributes.Action == "update" && payload.Changes.Draft != nil && payload.Changes.Draft.Previous && !payload.Changes.Draft.Current:
		event.Action = "ready_for_review"
	}
	reviewable := event.Action == "opened" || event.Action == "reopened" || event.Action == "synchronize" || event.Action == "ready_for_review"
	event.Eligible = reviewable && attributes.State == "opened" && !attributes.Draft && !attributes.WorkInProgress && event.HeadSHA != ""
	return event, nil
}

type forgejoHookPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		State string `json:"state"`
		Title string `json:"title"`
		Draft bool   `json:"draft"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	Changes struct {
		Title *struct {
			From string `json:"from"`
		} `json:"title"`
	} `json:"changes"`
}

func parseForgejo(body []byte) (Event, error) {
	var payload forgejoHookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return Event{}, err
	}
	pr := payload.PullRequest
	event := Event{
		Action:       payload.Action,
		Repository:   payload.Repository.FullName,
		RepositoryID: payload.Repository.ID,
		Number:       payload.Number,
		HeadSHA:      pr.Head.SHA,
		BaseSHA:      pr.Base.SHA,
	}
	if event.Repository == "" || event.Number < 1 {
		return Event{}, errors.New("pull request event missing repository or number")
	}
	draft := pr.Draft || workInProgress(pr.Title)
	switch payload.Action {
	case "synchronized":
		event.Action = "synchronize"
	case "edited":
		// Removing the WIP prefix from the title is how a draft is marked
		// ready on servers without draft pull requests.
		if payload.Changes.Title != nil && workInProgress(payload.Changes.Title.From) && !draft {
			event.Action = "ready_for_review"
		}
	}
	reviewable := event.Action == "opened" || event.Action == "reopened" || event.Action == "synchronize" || event.Action == "ready_for_review"
	event.Eligible = reviewable && pr.State == "open" && !draft && event.HeadSHA != "" && event.BaseSHA != ""
	return event, nil
}
