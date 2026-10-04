package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/daltoniam/overload"
)

type Local struct{}

func (Local) Start(_ context.Context, opts overload.SandboxOptions) (overload.Sandbox, error) {
	dir, err := os.MkdirTemp("", "overload-sandbox-")
	if err != nil {
		return nil, err
	}
	return &localHandle{dir: dir, name: fmt.Sprintf("local-%d", opts.RunID)}, nil
}

type localHandle struct {
	dir    string
	name   string
	mu     sync.Mutex
	closed bool
}

func (handle *localHandle) Name() string { return handle.name }

func (handle *localHandle) Upload(_ context.Context, name string, reader io.Reader) error {
	if !safeName(name) {
		return errors.New("invalid sandbox file name")
	}
	file, err := os.OpenFile(filepath.Join(handle.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	written, err := io.Copy(file, io.LimitReader(reader, (256<<20)+1))
	if err != nil || written > 256<<20 {
		_ = file.Close()
		_ = os.Remove(file.Name())
		if err != nil {
			return err
		}
		return errors.New("sandbox upload too large")
	}
	return file.Close()
}

func (handle *localHandle) Exec(ctx context.Context, command string) (overload.ExecResult, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = handle.dir
	output, err := cmd.CombinedOutput()
	result := overload.ExecResult{Stdout: string(output)}
	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

func (handle *localHandle) Download(_ context.Context, name string, writer io.Writer) (int64, error) {
	if !safeName(name) {
		return 0, errors.New("invalid sandbox file name")
	}
	path := filepath.Join(handle.dir, name)
	linkInfo, err := os.Lstat(path)
	if err != nil || !linkInfo.Mode().IsRegular() || linkInfo.Size() > 256<<20 {
		return 0, errors.New("invalid sandbox result file")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<20 {
		_ = file.Close()
		return 0, errors.New("invalid sandbox result file")
	}
	n, copyErr := io.Copy(writer, io.LimitReader(file, 256<<20))
	closeErr := file.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

func (handle *localHandle) Close(_ context.Context) error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.closed {
		return nil
	}
	if err := os.RemoveAll(handle.dir); err != nil {
		return err
	}
	handle.closed = true
	return nil
}
