package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/daltoniam/overload"
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

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
