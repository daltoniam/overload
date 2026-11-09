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

// GitLab talks to the GitLab REST API (v4) with a personal, group or
// project access token that has the api scope. Merge request IIDs play
// the part of pull request numbers.
type GitLab struct {
	api  *api
	mu   sync.Mutex
	self gitlabUser
}

type gitlabUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type gitlabDiffRefs struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	StartSHA string `json:"start_sha"`
}

type gitlabMergeRequest struct {
	IID             int            `json:"iid"`
	State           string         `json:"state"`
	Draft           bool           `json:"draft"`
	WorkInProgress  bool           `json:"work_in_progress"`
	SHA             string         `json:"sha"`
	TargetProjectID int64          `json:"target_project_id"`
	DiffRefs        gitlabDiffRefs `json:"diff_refs"`
}

func gitlabProject(repository string) (string, error) {
	if !overload.ValidForgeRepositoryName(overload.ForgeGitLab, repository) {
		return "", errors.New("invalid GitLab project path")
	}
	return "/projects/" + url.PathEscape(repository), nil
}

func (gitlab *GitLab) user(ctx context.Context) (gitlabUser, error) {
	gitlab.mu.Lock()
	defer gitlab.mu.Unlock()
	if gitlab.self.ID != 0 {
		return gitlab.self, nil
	}
	var user gitlabUser
	if _, err := gitlab.api.do(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return user, err
	}
	if user.ID == 0 {
		return user, errors.New("GitLab did not return the token's user")
	}
	gitlab.self = user
	return user, nil
}

func (gitlab *GitLab) Identity(ctx context.Context) (string, error) {
	user, err := gitlab.user(ctx)
	return user.Username, err
}

func (gitlab *GitLab) mergeRequest(ctx context.Context, repository string, number int) (string, gitlabMergeRequest, error) {
	project, err := gitlabProject(repository)
	if err != nil {
		return "", gitlabMergeRequest{}, err
	}
	if number < 1 {
		return "", gitlabMergeRequest{}, errors.New("merge request number must be positive")
	}
	path := project + "/merge_requests/" + strconv.Itoa(number)
	var mr gitlabMergeRequest
	_, err = gitlab.api.do(ctx, http.MethodGet, path, nil, &mr)
	return path, mr, err
}

// diffAttempts bounds how often a diff is fetched again when a push lands
// while it is read.
const diffAttempts = 3

// PullRequestWithDiff returns a merge request and its diff for the same
// head: the diff is read in pages, so the merge request is read again
// afterwards and the diff fetched again if a push moved the head.
func (gitlab *GitLab) PullRequestWithDiff(ctx context.Context, repository string, number int) (overload.PullRequest, string, error) {
	path, mr, err := gitlab.mergeRequest(ctx, repository, number)
	if err != nil {
		return overload.PullRequest{}, "", err
	}
	for range diffAttempts {
		patch, err := gitlab.diff(ctx, path)
		if err != nil {
			return overload.PullRequest{}, "", err
		}
		_, after, err := gitlab.mergeRequest(ctx, repository, number)
		if err != nil {
			return overload.PullRequest{}, "", err
		}
		if after.SHA == mr.SHA && after.DiffRefs == mr.DiffRefs {
			return gitlabPullRequest(mr), patch, nil
		}
		mr = after
	}
	return overload.PullRequest{}, "", fmt.Errorf("merge request !%d kept changing while its diff was read", number)
}

func gitlabPullRequest(mr gitlabMergeRequest) overload.PullRequest {
	state := mr.State
	if state == "opened" {
		state = "open"
	}
	pr := overload.PullRequest{Number: mr.IID, State: state, Draft: mr.Draft || mr.WorkInProgress, HeadSHA: mr.SHA, BaseSHA: mr.DiffRefs.BaseSHA, RepositoryID: mr.TargetProjectID}
	if pr.HeadSHA == "" {
		pr.HeadSHA = mr.DiffRefs.HeadSHA
	}
	return pr
}

type gitlabDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	AMode       string `json:"a_mode"`
	BMode       string `json:"b_mode"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
}

// diff rebuilds a git unified diff from the merge request's per-file
// diffs (GitLab 15.7 and later), which hold only the hunks.
func (gitlab *GitLab) diff(ctx context.Context, path string) (string, error) {
	var patch strings.Builder
	for page := 1; page <= 100; page++ {
		var files []gitlabDiff
		header, err := gitlab.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/diffs?per_page=100&page=%d", path, page), nil, &files)
		if err != nil {
			return "", err
		}
		for _, file := range files {
			patch.WriteString(unifiedDiff(file))
			if patch.Len() > maxDiffBytes {
				return "", fmt.Errorf("diff exceeds %d MiB", maxDiffBytes>>20)
			}
		}
		if next := header.Get("X-Next-Page"); next == "" || len(files) == 0 {
			break
		}
	}
	return patch.String(), nil
}

func unifiedDiff(file gitlabDiff) string {
	var out strings.Builder
	fmt.Fprintf(&out, "diff --git a/%s b/%s\n", file.OldPath, file.NewPath)
	from, to := "a/"+file.OldPath, "b/"+file.NewPath
	switch {
	case file.NewFile:
		fmt.Fprintf(&out, "new file mode %s\n", file.BMode)
		from = "/dev/null"
	case file.DeletedFile:
		fmt.Fprintf(&out, "deleted file mode %s\n", file.AMode)
		to = "/dev/null"
	case file.RenamedFile:
		fmt.Fprintf(&out, "rename from %s\nrename to %s\n", file.OldPath, file.NewPath)
	}
	if file.Diff == "" {
		return out.String()
	}
	fmt.Fprintf(&out, "--- %s\n+++ %s\n%s", from, to, file.Diff)
	if !strings.HasSuffix(file.Diff, "\n") {
		out.WriteString("\n")
	}
	return out.String()
}

func (gitlab *GitLab) DownloadHead(ctx context.Context, repository, sha string, writer io.Writer) error {
	project, err := gitlabProject(repository)
	if err != nil {
		return err
	}
	if !commitSHA.MatchString(sha) {
		return errors.New("invalid head SHA")
	}
	return gitlab.api.download(ctx, project+"/repository/archive.tar.gz?sha="+sha, writer)
}

type gitlabNote struct {
	ID     int64      `json:"id"`
	Body   string     `json:"body"`
	System bool       `json:"system"`
	Author gitlabUser `json:"author"`
}

func (gitlab *GitLab) FindReview(ctx context.Context, repository string, number int, marker string) (int64, error) {
	project, err := gitlabProject(repository)
	if err != nil {
		return 0, err
	}
	self, err := gitlab.user(ctx)
	if err != nil {
		return 0, err
	}
	for page := 1; page <= maxPages; page++ {
		var notes []gitlabNote
		header, err := gitlab.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d/notes?sort=desc&order_by=created_at&per_page=100&page=%d", project, number, page), nil, &notes)
		if err != nil {
			return 0, err
		}
		for _, note := range notes {
			if !note.System && note.Author.ID == self.ID && strings.Contains(note.Body, marker) {
				return note.ID, nil
			}
		}
		if header.Get("X-Next-Page") == "" || len(notes) == 0 {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("merge request notes: %w", errTooManyPages)
}

type gitlabVersion struct {
	HeadCommitSHA  string `json:"head_commit_sha"`
	BaseCommitSHA  string `json:"base_commit_sha"`
	StartCommitSHA string `json:"start_commit_sha"`
}

// diffRefs finds the merge request version whose head is sha, so inline
// comments land on the lines the review read even after a newer push.
func (gitlab *GitLab) diffRefs(ctx context.Context, path, sha string, mr gitlabMergeRequest) (gitlabDiffRefs, error) {
	if strings.EqualFold(mr.DiffRefs.HeadSHA, sha) {
		return mr.DiffRefs, nil
	}
	var versions []gitlabVersion
	if _, err := gitlab.api.do(ctx, http.MethodGet, path+"/versions?per_page=100", nil, &versions); err != nil {
		return gitlabDiffRefs{}, err
	}
	for _, version := range versions {
		if strings.EqualFold(version.HeadCommitSHA, sha) {
			return gitlabDiffRefs{BaseSHA: version.BaseCommitSHA, HeadSHA: version.HeadCommitSHA, StartSHA: version.StartCommitSHA}, nil
		}
	}
	return gitlabDiffRefs{}, fmt.Errorf("%w: commit %s is no longer part of the merge request", overload.ErrReviewRejected, sha)
}

// PostReview posts the summary as a merge request note (it carries the
// run marker, so a retry finds it), then one discussion per finding on
// its line. Findings GitLab cannot place on a line are listed in a
// follow-up note. APPROVE approves the merge request and REQUEST_CHANGES
// withdraws an earlier approval, when the token's account may do so.
func (gitlab *GitLab) PostReview(ctx context.Context, repository string, number int, sha, event, summary string, findings []overload.Finding) (int64, error) {
	path, mr, err := gitlab.mergeRequest(ctx, repository, number)
	if statusOf(err) == http.StatusNotFound {
		return 0, fmt.Errorf("%w: merge request not found", overload.ErrReviewRejected)
	}
	if err != nil {
		return 0, err
	}
	refs, err := gitlab.diffRefs(ctx, path, sha, mr)
	if err != nil {
		return 0, err
	}
	var note gitlabNote
	_, err = gitlab.api.do(ctx, http.MethodPost, path+"/notes", map[string]string{"body": summary}, &note)
	if status := statusOf(err); status == http.StatusNotFound || status == http.StatusUnprocessableEntity || status == http.StatusBadRequest {
		return 0, fmt.Errorf("%w: %v", overload.ErrReviewRejected, err)
	}
	if err != nil {
		return 0, err
	}
	var unplaced []overload.Finding
	for _, finding := range findings {
		position := map[string]any{
			"position_type": "text",
			"base_sha":      refs.BaseSHA,
			"start_sha":     refs.StartSHA,
			"head_sha":      refs.HeadSHA,
			"old_path":      finding.Path,
			"new_path":      finding.Path,
			"new_line":      finding.Line,
		}
		if _, err := gitlab.api.do(ctx, http.MethodPost, path+"/discussions", map[string]any{"body": commentBody(finding), "position": position}, nil); err != nil {
			slog.Warn("GitLab comment not placed on its line", "repository", repository, "merge_request", number, "path", finding.Path, "line", finding.Line, "error", err)
			unplaced = append(unplaced, finding)
		}
	}
	if len(unplaced) > 0 {
		var body strings.Builder
		body.WriteString("These findings could not be attached to their lines:\n")
		for _, finding := range unplaced {
			fmt.Fprintf(&body, "\n---\n\n`%s:%d` %s\n", finding.Path, finding.Line, commentBody(finding))
		}
		if _, err := gitlab.api.do(ctx, http.MethodPost, path+"/notes", map[string]string{"body": body.String()}, nil); err != nil {
			slog.Warn("GitLab note for unplaced findings failed", "repository", repository, "merge_request", number, "error", err)
		}
	}
	switch event {
	case overload.ReviewApprove:
		// Approve only the reviewed commit: GitLab refuses the approval when
		// the head has moved past sha, and a newer head is never approved.
		if !strings.EqualFold(mr.SHA, sha) {
			slog.Info("GitLab approval skipped: the merge request has a newer head than the review", "repository", repository, "merge_request", number)
			break
		}
		if _, err := gitlab.api.do(ctx, http.MethodPost, path+"/approve", map[string]string{"sha": sha}, nil); err != nil && statusOf(err) != http.StatusUnauthorized {
			slog.Warn("GitLab approval not given", "repository", repository, "merge_request", number, "error", err)
		}
	case overload.ReviewRequestChanges:
		if _, err := gitlab.api.do(ctx, http.MethodPost, path+"/unapprove", nil, nil); err != nil && statusOf(err) != http.StatusNotFound {
			slog.Warn("GitLab approval not withdrawn", "repository", repository, "merge_request", number, "error", err)
		}
	}
	return note.ID, nil
}

// ChangesRequested returns nothing: GitLab's REST API has no request for
// changes to withdraw. A blocking review shows as a failed commit status.
func (gitlab *GitLab) ChangesRequested(context.Context, string, int) ([]int64, error) {
	return nil, nil
}

func (gitlab *GitLab) DismissReview(context.Context, string, int, int64, string) error {
	return nil
}

// gitlabStates maps overload's commit status states to GitLab's.
var gitlabStates = map[string]string{"pending": "running", "success": "success", "failure": "failed", "error": "failed"}

func (gitlab *GitLab) SetStatus(ctx context.Context, repository, sha, state, description, targetURL string) error {
	project, err := gitlabProject(repository)
	if err != nil {
		return err
	}
	if !commitSHA.MatchString(sha) {
		return errors.New("invalid commit SHA")
	}
	mapped, ok := gitlabStates[state]
	if !ok {
		return fmt.Errorf("invalid status state %q", state)
	}
	body := map[string]string{"state": mapped, "name": StatusContext, "description": overload.TruncateUTF8(description, 255)}
	if targetURL != "" {
		body["target_url"] = targetURL
	}
	_, err = gitlab.api.do(ctx, http.MethodPost, project+"/statuses/"+sha, body, nil)
	switch status := statusOf(err); {
	case status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %v", overload.ErrStatusForbidden, err)
	case status == http.StatusBadRequest && strings.Contains(err.Error(), "Cannot transition"):
		// The status is already in that state.
		return nil
	}
	return err
}

type gitlabHook struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

func (gitlab *GitLab) CreateWebhook(ctx context.Context, repository, hookURL, secret string) error {
	project, err := gitlabProject(repository)
	if err != nil {
		return err
	}
	body := map[string]any{"url": hookURL, "token": secret, "merge_requests_events": true, "push_events": false, "enable_ssl_verification": strings.HasPrefix(hookURL, "https://")}
	for page := 1; ; page++ {
		if page > maxPages {
			return fmt.Errorf("project hooks: %w", errTooManyPages)
		}
		var hooks []gitlabHook
		header, err := gitlab.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/hooks?per_page=100&page=%d", project, page), nil, &hooks)
		if err != nil {
			return err
		}
		for _, hook := range hooks {
			if hook.URL == hookURL {
				_, err := gitlab.api.do(ctx, http.MethodPut, fmt.Sprintf("%s/hooks/%d", project, hook.ID), body, nil)
				return err
			}
		}
		if header.Get("X-Next-Page") == "" || len(hooks) == 0 {
			break
		}
	}
	_, err = gitlab.api.do(ctx, http.MethodPost, project+"/hooks", body, nil)
	return err
}
