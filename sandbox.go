package overload

import (
	"context"
	"io"
)

type SandboxOptions struct {
	RunID int64
}

type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type SandboxRunner interface {
	Start(context.Context, SandboxOptions) (Sandbox, error)
}

type Sandbox interface {
	Name() string
	Upload(context.Context, string, io.Reader) error
	Exec(context.Context, string) (ExecResult, error)
	Download(context.Context, string, io.Writer) (int64, error)
	Close(context.Context) error
}
