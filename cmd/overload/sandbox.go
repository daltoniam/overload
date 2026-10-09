package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/modelproxy"
	reviewpkg "github.com/daltoniam/overload/review"
	"github.com/daltoniam/overload/sandbox"
	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// sandboxFromEnvironment selects where webhook reviews run. The default
// ("host") reviews inside the overload process; "agent-sandbox" claims a pod
// from an Agent Sandbox warm pool for each review.
func sandboxFromEnvironment(ctx context.Context) (overload.SandboxRunner, error) {
	mode := os.Getenv("OVERLOAD_SANDBOX")
	switch mode {
	case "", "host":
		return nil, nil
	case "agent-sandbox":
		pool := envOr("OVERLOAD_SANDBOX_WARMPOOL", "overload-agent")
		namespace := envOr("OVERLOAD_SANDBOX_NAMESPACE", "overload")
		connectivity := sdk.Connectivity(envOr("OVERLOAD_SANDBOX_CONNECTIVITY", string(sdk.ConnectivityInClusterService)))
		runner, err := sandbox.NewAgent(ctx, pool, namespace, connectivity)
		if err != nil {
			return nil, fmt.Errorf("agent sandbox: %w", err)
		}
		slog.Info("webhook reviews run in agent sandboxes", "pool", pool, "namespace", namespace, "connectivity", connectivity)
		return runner, nil
	default:
		return nil, fmt.Errorf("OVERLOAD_SANDBOX must be host or agent-sandbox, got %q", mode)
	}
}

// modelProxyFromEnvironment starts the model proxy when
// OVERLOAD_MODEL_PROXY_URL is set: the address sandboxes use to reach this
// pod (for example http://overload.overload.svc:8083). It listens on
// OVERLOAD_MODEL_PROXY_ADDR (default 0.0.0.0:8083), separately from the UI,
// so network policy can open it to sandbox pods alone.
func modelProxyFromEnvironment(ctx context.Context, sandboxed bool) (reviewpkg.ModelGrants, error) {
	base := os.Getenv("OVERLOAD_MODEL_PROXY_URL")
	if base == "" {
		return nil, nil
	}
	if !sandboxed {
		return nil, errors.New("OVERLOAD_MODEL_PROXY_URL needs OVERLOAD_SANDBOX=agent-sandbox")
	}
	if parsed, err := url.Parse(base); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" {
		return nil, errors.New("OVERLOAD_MODEL_PROXY_URL must be a scheme and host such as http://overload.overload.svc:8083")
	}
	proxy := modelproxy.New(base)
	server := &http.Server{Addr: envOr("OVERLOAD_MODEL_PROXY_ADDR", "0.0.0.0:8083"), Handler: proxy, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: time.Minute, IdleTimeout: 2 * time.Minute}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return nil, fmt.Errorf("model proxy: %w", err)
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("model proxy stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("model proxy for sandboxes", "addr", server.Addr, "url", base)
	return proxy, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
