package sandbox

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

func TestLocalLifecycle(t *testing.T) {
	ctx := context.Background()
	handle, err := (Local{}).Start(ctx, overload.SandboxOptions{RunID: 123})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Upload(ctx, "spec.json", strings.NewReader(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Exec(ctx, "cp spec.json result.json")
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("exec: %+v, %v", result, err)
	}
	var resultFile bytes.Buffer
	if _, err := handle.Download(ctx, "result.json", &resultFile); err != nil {
		t.Fatal(err)
	}
	if resultFile.String() != `{"ok":true}` {
		t.Fatalf("unexpected result: %q", resultFile.String())
	}
	if err := handle.Upload(ctx, "../escape", strings.NewReader("x")); err == nil {
		t.Fatal("accepted unsafe path")
	}
	if result, err := handle.Exec(ctx, "ln -s /etc/passwd outside"); err != nil || result.ExitCode != 0 {
		t.Fatalf("symlink setup: %+v, %v", result, err)
	}
	if _, err := handle.Download(ctx, "outside", &resultFile); err == nil {
		t.Fatal("accepted symlink result")
	}
	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(ctx); err != nil {
		t.Fatal("close must be idempotent", err)
	}
	if _, err := handle.Download(ctx, "result.json", &resultFile); err == nil {
		t.Fatal("result remains after cleanup")
	}
}
