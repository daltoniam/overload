package sandbox

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daltoniam/overload"
	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

type AgentSandbox struct {
	Client    *sdk.Client
	WarmPool  string
	Namespace string
}

func NewAgent(ctx context.Context, pool, namespace string, connectivity sdk.Connectivity) (*AgentSandbox, error) {
	client, err := sdk.NewClient(ctx, sdk.Options{Runtime: sdk.RuntimeSandboxd, Connectivity: connectivity})
	if err != nil {
		return nil, err
	}
	return &AgentSandbox{Client: client, WarmPool: pool, Namespace: namespace}, nil
}

func (runner *AgentSandbox) Start(ctx context.Context, _ overload.SandboxOptions) (overload.Sandbox, error) {
	handle, err := runner.Client.CreateSandbox(ctx, runner.WarmPool, runner.Namespace)
	if err != nil {
		return nil, err
	}
	return &agentHandle{sandbox: handle}, nil
}

type agentHandle struct {
	sandbox *sdk.Sandbox
	mu      sync.Mutex
	closed  bool
}

func (handle *agentHandle) Name() string { return handle.sandbox.ClaimName() }

func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, `/\`)
}

func (handle *agentHandle) Upload(ctx context.Context, name string, reader io.Reader) error {
	if !safeName(name) {
		return errors.New("invalid sandbox file name")
	}
	return handle.sandbox.WriteReader(ctx, "/work/"+name, reader)
}

func (handle *agentHandle) Exec(ctx context.Context, command string) (overload.ExecResult, error) {
	result, err := handle.sandbox.Run(ctx, command)
	if err != nil {
		return overload.ExecResult{}, err
	}
	return overload.ExecResult{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}, nil
}

func (handle *agentHandle) Download(ctx context.Context, name string, writer io.Writer) (int64, error) {
	if !safeName(name) {
		return 0, errors.New("invalid sandbox file name")
	}
	return handle.sandbox.ReadTo(ctx, "/work/"+name, writer)
}

func (handle *agentHandle) Close(_ context.Context) error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.closed {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := handle.sandbox.Close(cleanupCtx); err != nil {
		return err
	}
	handle.closed = true
	return nil
}
