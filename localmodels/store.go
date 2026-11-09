package localmodels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/daltoniam/overload/catalog"
)

// Store keeps downloaded weights under Dir, one directory per repository,
// and a manifest of what finished downloading and verified. Dir must be
// absolute: launchd services refer to files by path.
type Store struct {
	Dir string
	now func() time.Time
}

// NewStore returns a store rooted at dir.
func NewStore(dir string) Store {
	return Store{Dir: dir, now: time.Now}
}

// Installed is one downloaded quantization of a catalog model.
type Installed struct {
	ID         string `json:"id"`
	Quant      string `json:"quant"`
	Repository string `json:"repository"`
	// Revision is the repository commit the files came from.
	Revision     string    `json:"revision,omitempty"`
	Files        []File    `json:"files"`
	DownloadedAt time.Time `json:"downloaded_at"`
}

// Size is the bytes the installed files take.
func (installed Installed) Size() int64 {
	var total int64
	for _, file := range installed.Files {
		total += file.Size
	}
	return total
}

// Progress reports bytes done so far out of total for one file; verifying
// is set while an existing file is checked rather than downloaded.
type Progress func(path string, done, total int64, verifying bool)

const (
	partialSuffix = ".part"
	defaultStall  = 2 * time.Minute
	// freeMargin is left free on the volume after a download.
	freeMargin = 2 << 30
)

// Pull downloads quant of the catalog model from repository (the entry's
// repository for that quant when empty), at the repository's current
// revision. Partial files resume; every file is verified against its
// published SHA-256, and files already verified are not downloaded again.
func (store Store) Pull(ctx context.Context, hub Hub, model catalog.Model, quant, repository string, progress Progress) (Installed, error) {
	if repository == "" {
		for _, weights := range model.Weights {
			if strings.EqualFold(weights.Quant, quant) && weights.Repository != "" {
				repository, quant = weights.Repository, weights.Quant
			}
		}
	}
	if repository == "" {
		return Installed{}, fmt.Errorf("the catalog has no download for %s %s; pass a Hugging Face repository with --repository", model.ID, quant)
	}
	if !ValidRepository(repository) {
		return Installed{}, fmt.Errorf("invalid repository %q", repository)
	}
	if !filepath.IsAbs(store.Dir) {
		return Installed{}, errors.New("the model directory must be an absolute path")
	}
	revision, err := hub.Revision(ctx, repository)
	if err != nil {
		return Installed{}, err
	}
	files, err := hub.ListFiles(ctx, repository, revision)
	if err != nil {
		return Installed{}, err
	}
	selected, err := SelectQuant(files, quant)
	if err != nil {
		return Installed{}, fmt.Errorf("%s: %w", repository, err)
	}
	unlock, err := store.lock(".lock")
	if err != nil {
		return Installed{}, err
	}
	defer unlock()
	if err := store.checkSpace(repository, selected); err != nil {
		return Installed{}, err
	}
	for _, file := range selected {
		if err := store.download(ctx, hub, repository, revision, file, progress); err != nil {
			return Installed{}, fmt.Errorf("%s: %w", file.Path, err)
		}
	}
	installed := Installed{ID: model.ID, Quant: quant, Repository: repository, Revision: revision, Files: selected, DownloadedAt: store.now().UTC()}
	manifest, err := store.readManifest()
	if err != nil {
		return Installed{}, err
	}
	manifest = slices.DeleteFunc(manifest, func(entry Installed) bool { return entry.ID == installed.ID && strings.EqualFold(entry.Quant, quant) })
	manifest = append(manifest, installed)
	return installed, store.writeManifest(manifest)
}

// checkSpace refuses a download that would leave less than freeMargin.
func (store Store) checkSpace(repository string, files []File) error {
	var needed int64
	for _, file := range files {
		needed += file.Size
		if info, err := os.Stat(store.partialPath(repository, file)); err == nil {
			needed -= info.Size()
		}
		if info, err := os.Stat(store.Path(repository, file.Path)); err == nil && info.Size() == file.Size {
			needed -= file.Size
		}
	}
	free, err := freeBytes(store.Dir)
	if err != nil || needed <= 0 {
		return nil
	}
	if free-needed < freeMargin {
		return fmt.Errorf("the download needs %.1f GB but only %.1f GB is free on the volume; free space or run overload models clean", float64(needed)/1e9, float64(free)/1e9)
	}
	return nil
}

// Path is where a repository file is stored.
func (store Store) Path(repository, path string) string {
	return filepath.Join(store.Dir, filepath.FromSlash(repository), filepath.FromSlash(path))
}

// partialPath names an unfinished download after the digest it should
// end with, so a partial file from another revision is never resumed.
func (store Store) partialPath(repository string, file File) string {
	return store.Path(repository, file.Path) + "." + file.SHA256[:16] + partialSuffix
}

func (store Store) download(ctx context.Context, hub Hub, repository, revision string, file File, progress Progress) error {
	if !safeRelative(file.Path) || !digestPattern.MatchString(file.SHA256) {
		return errors.New("invalid file entry")
	}
	destination := store.Path(repository, file.Path)
	if info, err := os.Stat(destination); err == nil && info.Size() == file.Size {
		if progress != nil {
			progress(file.Path, 0, file.Size, true)
		}
		if sum, err := fileDigest(destination); err == nil && sum == file.SHA256 {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	partial := store.partialPath(repository, file)
	stale, _ := filepath.Glob(destination + ".*" + partialSuffix)
	for _, old := range stale {
		if old != partial {
			_ = os.Remove(old)
		}
	}
	if info, err := os.Stat(partial); err == nil && info.Size() > 0 && progress != nil {
		progress(file.Path, info.Size(), file.Size, true)
	}
	output, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = output.Close() }()
	digest := sha256.New()
	offset, err := io.Copy(digest, output)
	if err != nil {
		return err
	}
	if offset > file.Size {
		if err := restart(output, digest); err != nil {
			return err
		}
		offset = 0
	}
	if offset < file.Size {
		offset, err = fetch(ctx, hub, hub.fileURL(repository, revision, file.Path), file, output, digest, offset, progress)
		if err != nil {
			return err
		}
	}
	if offset != file.Size {
		return fmt.Errorf("download stopped at %d of %d bytes; run the command again to resume", offset, file.Size)
	}
	if hex.EncodeToString(digest.Sum(nil)) != file.SHA256 {
		_ = output.Close()
		_ = os.Remove(partial)
		return errors.New("downloaded file does not match its published SHA-256; the partial file was removed")
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(partial, destination)
}

// fetch downloads file from offset into output and digest and returns the
// bytes now written. A server that ignores or cannot satisfy the range
// gets one request for the whole file instead.
func fetch(ctx context.Context, hub Hub, path string, file File, output *os.File, digest hash.Hash, offset int64, progress Progress) (int64, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	request, err := hub.request(ctx, path)
	if err != nil {
		return offset, err
	}
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	response, err := hub.client().Do(request)
	if err != nil {
		return offset, requestError(err)
	}
	defer func() { _ = response.Body.Close() }()
	switch {
	case response.StatusCode == http.StatusPartialContent && offset > 0 && rangeMatches(response.Header.Get("Content-Range"), offset, file.Size):
	case response.StatusCode == http.StatusOK:
		if err := restart(output, digest); err != nil {
			return offset, err
		}
		offset = 0
	case offset > 0 && (response.StatusCode == http.StatusRequestedRangeNotSatisfiable || response.StatusCode == http.StatusPartialContent):
		_ = response.Body.Close()
		if err := restart(output, digest); err != nil {
			return offset, err
		}
		return fetch(ctx, hub, path, file, output, digest, 0, progress)
	default:
		return offset, errors.New(statusMessage(response))
	}
	stall := hub.Stall
	if stall <= 0 {
		stall = defaultStall
	}
	watchdog := time.AfterFunc(stall, func() { cancel(fmt.Errorf("no data received for %s", stall)) })
	defer watchdog.Stop()
	reader := io.LimitReader(response.Body, file.Size-offset)
	buffer := make([]byte, 1<<20)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			watchdog.Reset(stall)
			if _, err := output.Write(buffer[:n]); err != nil {
				return offset, err
			}
			digest.Write(buffer[:n])
			offset += int64(n)
			if progress != nil {
				progress(file.Path, offset, file.Size, false)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return offset, nil
		}
		if readErr != nil {
			if cause := context.Cause(ctx); cause != nil {
				readErr = cause
			}
			return offset, fmt.Errorf("download interrupted at %d of %d bytes (run the command again to resume): %w", offset, file.Size, requestError(readErr))
		}
	}
}

// rangeMatches checks a Content-Range of "bytes start-end/total".
func rangeMatches(header string, offset, size int64) bool {
	value, ok := strings.CutPrefix(header, "bytes ")
	if !ok {
		return false
	}
	span, total, ok := strings.Cut(value, "/")
	start, _, ok2 := strings.Cut(span, "-")
	if !ok || !ok2 || start != strconv.FormatInt(offset, 10) {
		return false
	}
	return total == "*" || total == strconv.FormatInt(size, 10)
}

func restart(output *os.File, digest hash.Hash) error {
	if err := output.Truncate(0); err != nil {
		return err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest.Reset()
	return nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// List returns the installed models, oldest download first.
func (store Store) List() ([]Installed, error) {
	return store.readManifest()
}

// Find returns the installed quant of id; with an empty quant, the only
// installed quant.
func (store Store) Find(id, quant string) (Installed, error) {
	manifest, err := store.readManifest()
	if err != nil {
		return Installed{}, err
	}
	var matches []Installed
	for _, entry := range manifest {
		if entry.ID == id && (quant == "" || strings.EqualFold(entry.Quant, quant)) {
			matches = append(matches, entry)
		}
	}
	switch len(matches) {
	case 0:
		return Installed{}, fmt.Errorf("%s %s is not downloaded; run overload models pull", id, quant)
	case 1:
		return matches[0], nil
	default:
		return Installed{}, fmt.Errorf("%s has more than one quantization downloaded; choose one with --quant", id)
	}
}

// Remove deletes an installed quantization's files, keeping any another
// installed entry still uses. A model that is being served cannot be
// removed.
func (store Store) Remove(id, quant string) (Installed, error) {
	unlock, err := store.lock(".lock")
	if err != nil {
		return Installed{}, err
	}
	defer unlock()
	unlockServices, err := store.lock(".services.lock")
	if err != nil {
		return Installed{}, err
	}
	defer unlockServices()
	installed, err := store.Find(id, quant)
	if err != nil {
		return Installed{}, err
	}
	services, err := store.Services()
	if err != nil {
		return Installed{}, err
	}
	for _, service := range services {
		if service.ID == installed.ID && strings.EqualFold(service.Quant, installed.Quant) {
			return Installed{}, fmt.Errorf("%s is being served; run overload models stop %s first", id, id)
		}
	}
	manifest, err := store.readManifest()
	if err != nil {
		return Installed{}, err
	}
	manifest = slices.DeleteFunc(manifest, func(entry Installed) bool {
		return entry.ID == installed.ID && entry.Quant == installed.Quant
	})
	inUse := map[string]bool{}
	for _, entry := range manifest {
		for _, file := range entry.Files {
			inUse[store.Path(entry.Repository, file.Path)] = true
		}
	}
	for _, file := range installed.Files {
		path := store.Path(installed.Repository, file.Path)
		if inUse[path] {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Installed{}, err
		}
		store.removeEmptyParents(filepath.Dir(path))
	}
	return installed, store.writeManifest(manifest)
}

// removeEmptyParents deletes empty directories up to, but not including,
// the store's root.
func (store Store) removeEmptyParents(dir string) {
	for dir != store.Dir && strings.HasPrefix(dir, store.Dir+string(filepath.Separator)) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// Clean removes partial downloads and returns the bytes freed.
func (store Store) Clean() (int64, error) {
	unlock, err := store.lock(".lock")
	if err != nil {
		return 0, err
	}
	defer unlock()
	var freed int64
	err = filepath.WalkDir(store.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), partialSuffix) {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			freed += info.Size()
		}
		return nil
	})
	return freed, err
}

// Usage returns the bytes the store's files take, including partial
// downloads, and the bytes free on its volume.
func (store Store) Usage() (used, free int64, err error) {
	err = filepath.WalkDir(store.Dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			used += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	free, err = freeBytes(store.Dir)
	return used, free, err
}

func freeBytes(dir string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(existingParent(dir), &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func existingParent(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil || filepath.Dir(dir) == dir {
			return dir
		}
		dir = filepath.Dir(dir)
	}
}

func (store Store) manifestPath() string { return filepath.Join(store.Dir, "manifest.json") }

func (store Store) readManifest() ([]Installed, error) {
	data, err := os.ReadFile(store.manifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var manifest []Installed
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("read %s: %w", store.manifestPath(), err)
	}
	return manifest, nil
}

func (store Store) writeManifest(manifest []Installed) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(store.manifestPath(), data)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// lock takes an exclusive lock on a file in the store's directory, so two
// commands do not write the same files.
func (store Store) lock(name string) (func(), error) {
	if err := os.MkdirAll(store.Dir, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(store.Dir, name), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another overload models command is using the model directory")
		}
		return nil, fmt.Errorf("lock the model directory: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
