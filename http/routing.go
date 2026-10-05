package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/web/templates/pages"
)

// RunRoutingReader is implemented by stores that record which agent
// reviewed which files.
type RunRoutingReader interface {
	ReadRunRouting(context.Context, int64) (overload.Routing, error)
}

// ChangedFilesSource is implemented by stores that can list the files a
// pull request changes, so a workflow can be previewed on a real PR.
type ChangedFilesSource interface {
	ChangedFiles(ctx context.Context, repository string, number int) ([]string, error)
}

func registerWorkflowPreview(mux *http.ServeMux, store ConfigurationStore) {
	source, canFetch := store.(ChangedFilesSource)
	mux.HandleFunc("GET /configure/workflows/{name}/preview", func(w http.ResponseWriter, r *http.Request) {
		workflow, err := store.ResolveWorkflow(r.Context(), r.PathValue("name"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		form := pages.PreviewForm{Files: query.Get("files"), Repository: query.Get("repository"), PullRequest: query.Get("pr"), CanFetch: canFetch}
		if len(form.Files) > 256<<10 {
			http.Error(w, "Changed file list is too long", http.StatusRequestEntityTooLarge)
			return
		}
		if repos, err := store.ListRepositories(r.Context()); err == nil {
			for _, repo := range repos {
				form.Repositories = append(form.Repositories, repo.FullName)
			}
		}
		var preview *overload.Preview
		message := ""
		switch {
		case workflow.Kind != "pr_review":
			message = "Only pull request review workflows route changed files."
		case form.PullRequest != "":
			number, err := strconv.Atoi(form.PullRequest)
			if err != nil || number < 1 || !canFetch || form.Repository == "" {
				message = "Choose a repository and a pull request number."
				break
			}
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			paths, err := source.ChangedFiles(ctx, form.Repository, number)
			cancel()
			if err != nil {
				message = "Could not load the pull request: " + err.Error()
				break
			}
			if len(paths) > 3000 {
				paths = paths[:3000]
			}
			form.Files = strings.Join(paths, "\n")
			result, _ := workflow.Preview(paths)
			preview = &result
		case form.Files != "":
			paths, err := overload.ParseChangedFiles(form.Files)
			if err != nil {
				message = err.Error()
				break
			}
			result, _ := workflow.Preview(paths)
			preview = &result
		}
		_ = pages.WorkflowPreview(workflow, form, preview, message).Render(r.Context(), w)
	})
}
