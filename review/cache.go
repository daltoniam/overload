package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"
)

var cacheSHA = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

// ArchiveCache keeps head archives on the control plane's disk by
// repository and commit, so reviews of the same head (an evaluation of
// several workflows, a review asked for again) download it once. The
// oldest archives are removed when the cache passes MaxBytes. Archives
// are never extracted on the control plane.
type ArchiveCache struct {
	Dir      string
	MaxBytes int64

	mu sync.Mutex
}

func (cache *ArchiveCache) path(repository, sha string) string {
	sum := sha256.Sum256([]byte(repository))
	return filepath.Join(cache.Dir, hex.EncodeToString(sum[:8]), sha+".tar.gz")
}

// CachedSource reads head archives through cache and everything else from
// Source.
type CachedSource struct {
	PullRequestSource
	Cache *ArchiveCache
}

// DownloadHead copies a cached archive, or downloads it into the cache
// first. A cache failure falls back to the source.
func (source CachedSource) DownloadHead(ctx context.Context, repository, sha string, writer io.Writer) error {
	if source.Cache == nil || source.Cache.Dir == "" || !cacheSHA.MatchString(sha) {
		return source.PullRequestSource.DownloadHead(ctx, repository, sha, writer)
	}
	name := source.Cache.path(repository, sha)
	if file, err := os.Open(name); err == nil {
		defer func() { _ = file.Close() }()
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		_ = os.Chtimes(name, now, now)
		_, err := io.Copy(writer, contextReader{ctx: ctx, reader: file})
		return err
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return source.PullRequestSource.DownloadHead(ctx, repository, sha, writer)
	}
	temporary, err := os.CreateTemp(filepath.Dir(name), ".download-*")
	if err != nil {
		return source.PullRequestSource.DownloadHead(ctx, repository, sha, writer)
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	copy := &bestEffort{file: temporary}
	if err := source.PullRequestSource.DownloadHead(ctx, repository, sha, io.MultiWriter(copy, writer)); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err == nil && copy.err == nil && os.Rename(temporary.Name(), name) == nil {
		source.Cache.trim()
	}
	return nil
}

// contextReader stops a copy once its context is done.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buf []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buf)
}

// bestEffort writes to the cache file until a write fails (a full disk),
// then drops the rest without failing the download it is copied from.
type bestEffort struct {
	file *os.File
	err  error
}

func (copy *bestEffort) Write(data []byte) (int, error) {
	if copy.err == nil {
		_, copy.err = copy.file.Write(data)
	}
	return len(data), nil
}

// trim removes the least recently used archives until the cache fits.
func (cache *ArchiveCache) trim() {
	if cache.MaxBytes <= 0 {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	type entry struct {
		path string
		size int64
		used time.Time
	}
	var entries []entry
	var total int64
	_ = filepath.WalkDir(cache.Dir, func(name string, item fs.DirEntry, err error) error {
		if err != nil || item.IsDir() || filepath.Ext(name) != ".gz" {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, entry{path: name, size: info.Size(), used: info.ModTime()})
		total += info.Size()
		return nil
	})
	slices.SortFunc(entries, func(a, b entry) int { return a.used.Compare(b.used) })
	for _, item := range entries {
		if total <= cache.MaxBytes {
			return
		}
		if err := os.Remove(item.path); err == nil || errors.Is(err, fs.ErrNotExist) {
			total -= item.size
		}
	}
}
