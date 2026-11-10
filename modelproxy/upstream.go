package modelproxy

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/daltoniam/overload"
)

// Upstream sends model requests with a saved model's own credentials. The
// model proxy and the gateway share it so the headers and credentials that
// reach a model server cannot drift apart.
type Upstream struct {
	client    *http.Client
	userAgent string
}

// NewUpstream returns an Upstream that identifies itself as userAgent.
func NewUpstream(userAgent string) *Upstream {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Never follow redirects: Go keeps X-Api-Key on a redirect to another
	// host, and the caller should not choose where the key goes.
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Upstream{client: client, userAgent: userAgent}
}

// Send posts body to path under the model's base URL. From the incoming
// headers only Accept and, for Anthropic, Anthropic-Version are kept; the
// caller's credentials never reach the model server.
func (upstream *Upstream) Send(ctx context.Context, model overload.ModelProfile, path string, incoming http.Header, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(model.BaseURL, "/")+"/"+strings.TrimLeft(path, "/"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if accept := incoming.Get("Accept"); accept != "" {
		request.Header.Set("Accept", accept)
	}
	request.Header.Set("User-Agent", upstream.userAgent)
	for name, value := range model.Headers {
		request.Header.Set(name, value)
	}
	switch {
	case model.Provider == overload.ProviderAnthropic:
		// Anthropic-Beta is not forwarded: some beta features (the MCP
		// connector) would let a caller make Anthropic reach URLs of its
		// choosing with overload's key.
		if value := incoming.Get("Anthropic-Version"); value != "" {
			request.Header.Set("Anthropic-Version", value)
		}
		if model.APIKeyEnv != "" {
			request.Header.Set("X-Api-Key", os.Getenv(model.APIKeyEnv))
		}
	case model.APIKeyEnv != "":
		request.Header.Set("Authorization", "Bearer "+os.Getenv(model.APIKeyEnv))
	}
	return upstream.client.Do(request)
}

// Filter rewrites a response body as it streams through Relay. Process
// returns the bytes to send for the next chunk read; Finish returns any
// bytes still held when the body ends, and is called even when the client
// has gone.
type Filter interface {
	Process(chunk []byte) []byte
	Finish() []byte
}

// Relay copies a model response to w with status: only its Content-Type and
// Retry-After headers, and the body as it arrives, through filter when set.
func Relay(w http.ResponseWriter, response *http.Response, status int, filter Filter) {
	for _, name := range []string{"Content-Type", "Retry-After"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)
	send := func(data []byte) bool {
		if len(data) == 0 {
			return true
		}
		if _, err := w.Write(data); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	buf := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if filter != nil {
				chunk = filter.Process(chunk)
			}
			if !send(chunk) {
				if filter != nil {
					filter.Finish()
				}
				return
			}
		}
		if readErr != nil {
			if filter != nil {
				send(filter.Finish())
			}
			return
		}
	}
}
