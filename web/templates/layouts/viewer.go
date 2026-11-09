package layouts

import "context"

// Viewer is the person signed in with OIDC, shown in the sidebar with a
// sign-out button. Installs using Basic Auth, Cloudflare Access or no
// sign-in have no viewer.
type Viewer struct {
	Email string
	Role  string
	// CSRF is the form token the sign-out form posts.
	CSRF string
}

type viewerKey struct{}

// WithViewer stores the signed-in person for the layout.
func WithViewer(ctx context.Context, viewer Viewer) context.Context {
	return context.WithValue(ctx, viewerKey{}, viewer)
}

// ViewerFrom returns the signed-in person, if any.
func ViewerFrom(ctx context.Context) (Viewer, bool) {
	viewer, ok := ctx.Value(viewerKey{}).(Viewer)
	return viewer, ok
}
