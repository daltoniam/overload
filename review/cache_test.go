package review

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type countingDownloads struct {
	fakePRSource
	count int
}

func (source *countingDownloads) DownloadHead(_ context.Context, _, _ string, writer io.Writer) error {
	source.count++
	_, err := writer.Write(source.archive)
	return err
}

func TestCachedSourceDownloadsEachHeadOnce(t *testing.T) {
	dir := t.TempDir()
	inner := &countingDownloads{fakePRSource: fakePRSource{archive: bytes.Repeat([]byte("a"), 1000)}}
	cache := &ArchiveCache{Dir: dir, MaxBytes: 2500}
	source := CachedSource{PullRequestSource: inner, Cache: cache}
	head := func(sha string) []byte {
		var out bytes.Buffer
		if err := source.DownloadHead(context.Background(), "acme/api", sha, &out); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	first := strings.Repeat("a", 40)
	if got := head(first); len(got) != 1000 || inner.count != 1 {
		t.Fatalf("first download: %d bytes, %d downloads", len(got), inner.count)
	}
	if got := head(first); len(got) != 1000 || inner.count != 1 {
		t.Fatalf("cached download: %d bytes, %d downloads", len(got), inner.count)
	}
	head(strings.Repeat("b", 40))
	head(strings.Repeat("c", 40))
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.tar.gz"))
	if len(files) != 2 {
		t.Fatalf("cache kept %d archives over its size limit", len(files))
	}
	if _, err := os.Stat(cache.path("acme/api", first)); err == nil {
		t.Fatal("least recently used archive kept")
	}
	if head("not-a-sha"); inner.count != 4 {
		t.Fatalf("invalid commit went through the cache: %d downloads", inner.count)
	}
}

type cancelAfterWrite struct {
	cancel  context.CancelFunc
	written int
}

func (writer *cancelAfterWrite) Write(data []byte) (int, error) {
	writer.written += len(data)
	writer.cancel()
	return len(data), nil
}

func TestCacheHitHonorsCancellation(t *testing.T) {
	inner := &countingDownloads{fakePRSource: fakePRSource{archive: bytes.Repeat([]byte("a"), 200_000)}}
	source := CachedSource{PullRequestSource: inner, Cache: &ArchiveCache{Dir: t.TempDir()}}
	sha := strings.Repeat("e", 40)
	if err := source.DownloadHead(context.Background(), "acme/api", sha, io.Discard); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := source.DownloadHead(canceled, "acme/api", sha, io.Discard); err == nil {
		t.Fatal("a canceled request copied the cached archive")
	}
	// Canceled mid-copy: the copy stops after the current read.
	ctx, cancel := context.WithCancel(context.Background())
	writer := &cancelAfterWrite{cancel: cancel}
	if err := source.DownloadHead(ctx, "acme/api", sha, writer); err == nil || writer.written >= 200_000 {
		t.Fatalf("copy went on after cancellation: %d bytes, %v", writer.written, err)
	}
	if inner.count != 1 {
		t.Fatalf("downloads = %d", inner.count)
	}
}
