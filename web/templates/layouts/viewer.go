package layouts

import (
	"context"

	"github.com/daltoniam/overload"
)

// Viewer is the person signed in with OIDC, shown in the sidebar with a
// sign-out button. Installs using Basic Auth, Cloudflare Access or no
// sign-in have no viewer.
type Viewer struct {
	// Name is the person's email, else their name or provider subject.
	Name string
	Role string
	// CSRF is the form token the sign-out form posts.
	CSRF string
}

type viewerKey struct{}

// WithViewer stores the signed-in person for the layout.
func WithViewer(ctx context.Context, viewer Viewer) context.Context {
	return context.WithValue(ctx, viewerKey{}, viewer)
}

// IsAdmin reports whether the person can change settings.
func (viewer Viewer) IsAdmin() bool { return viewer.Role == overload.RoleAdmin }

// WithoutViewer hides the signed-in person, for a page shown as they sign
// out.
func WithoutViewer(ctx context.Context) context.Context {
	return context.WithValue(ctx, viewerKey{}, Viewer{})
}

// ViewerFrom returns the signed-in person, if any.
func ViewerFrom(ctx context.Context) (Viewer, bool) {
	viewer, ok := ctx.Value(viewerKey{}).(Viewer)
	return viewer, ok && viewer.Role != ""
}
