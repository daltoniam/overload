package httpapi

import (
	"net/http"

	"github.com/daltoniam/overload/tunnel"
	"github.com/daltoniam/overload/web/templates/pages"
)

// TunnelControl sets up the Cloudflare Tunnel that exposes the webhook.
type TunnelControl interface {
	Status() tunnel.Status
	Login() error
	Setup(name, hostname string) error
	Stop() error
}

// TunnelProvider is implemented by stores that can manage a tunnel.
type TunnelProvider interface {
	Tunnel() TunnelControl
}

func registerTunnel(mux *http.ServeMux, control TunnelControl, csrf string) {
	render := func(w http.ResponseWriter, r *http.Request, fragment bool) {
		w.Header().Set("Cache-Control", "no-store")
		if fragment {
			_ = pages.TunnelStatus(control.Status(), csrf, "").Render(r.Context(), w)
			return
		}
		_ = pages.TunnelSetup(control.Status(), csrf, "").Render(r.Context(), w)
	}
	mux.HandleFunc("GET /setup/tunnel", func(w http.ResponseWriter, r *http.Request) { render(w, r, false) })
	mux.HandleFunc("GET /setup/tunnel/status", func(w http.ResponseWriter, r *http.Request) { render(w, r, true) })
	act := func(action func(r *http.Request) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !validForm(w, r, csrf) {
				return
			}
			if err := action(r); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Redirect(w, r, "/setup/tunnel", http.StatusSeeOther)
		}
	}
	mux.HandleFunc("POST /setup/tunnel/login", act(func(*http.Request) error { return control.Login() }))
	mux.HandleFunc("POST /setup/tunnel", act(func(r *http.Request) error {
		return control.Setup(r.PostForm.Get("name"), r.PostForm.Get("hostname"))
	}))
	mux.HandleFunc("POST /setup/tunnel/stop", act(func(*http.Request) error { return control.Stop() }))
}
