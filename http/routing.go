package httpapi

import (
	"context"
	"net/http"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/web/templates/pages"
)

// RunRoutingReader is implemented by stores that record which agent
// reviewed which files.
type RunRoutingReader interface {
	ReadRunRouting(context.Context, int64) (overload.Routing, error)
}

func registerWorkflowPreview(mux *http.ServeMux, store ConfigurationStore) {
	mux.HandleFunc("GET /configure/workflows/{name}/preview", func(w http.ResponseWriter, r *http.Request) {
		workflow, err := store.ResolveWorkflow(r.Context(), r.PathValue("name"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		files := r.URL.Query().Get("files")
		if len(files) > 256<<10 {
			http.Error(w, "Changed file list is too long", http.StatusRequestEntityTooLarge)
			return
		}
		var preview *overload.Preview
		message := ""
		switch {
		case workflow.Kind != "pr_review":
			message = "Only pull request review workflows route changed files."
		case files != "":
			paths, err := overload.ParseChangedFiles(files)
			if err != nil {
				message = err.Error()
				break
			}
			result, _ := workflow.Preview(paths)
			preview = &result
		}
		_ = pages.WorkflowPreview(workflow, files, preview, message).Render(r.Context(), w)
	})
}
