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
	// Freed is the bytes a pull deleted from the entry it replaced; it is
	// not stored.
	Freed int64 `json:"-"`
	// Served is set when a pull replaced an entry a model service uses;
	// the old files are kept until it is served again.
	Served bool `json:"-"`
	// Warning describes a clean-up problem after a successful pull.
	Warning string `json:"-"`
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
	installed := Installed{ID: model.ID, Quant: quant, Repository: repository, Revision: revision, Files: selected}
	for _, file := range selected {
		store.removeStalePartials(installed, file)
	}
	manifest, err := store.readManifest()
	if err != nil {
		return Installed{}, err
	}
	present := store.presentFiles(manifest, installed, progress)
	if err := store.checkSpace(installed, present); err != nil {
		return Installed{}, err
	}
	// Each revision has its own directory, so a new revision never
	// overwrites the files an installed entry or a running service uses;
	// the manifest switches to it only after every file is in place.
	for _, file := range selected {
		if present[file.Path] {
			continue
		}
		partial, err := store.download(ctx, hub, installed, file, progress)
		if err != nil {
			return Installed{}, fmt.Errorf("%s: %w", file.Path, err)
		}
		if err := os.Rename(partial, store.FilePath(installed, file)); err != nil {
			return Installed{}, err
		}
	}
	unlockServices, err := store.lock(".services.lock")
	if err != nil {
		return Installed{}, err
	}
	defer unlockServices()
	if manifest, err = store.readManifest(); err != nil {
		return Installed{}, err
	}
	installed.DownloadedAt = store.now().UTC()
	var replaced []Installed
	manifest = slices.DeleteFunc(manifest, func(entry Installed) bool {
		if entry.ID == installed.ID && strings.EqualFold(entry.Quant, quant) {
			replaced = append(replaced, entry)
			return true
		}
		return false
	})
	manifest = append(manifest, installed)
	if err := store.writeManifest(manifest); err != nil {
		return Installed{}, err
	}
	freed, retired, err := store.release(manifest, replaced)
	installed.Freed, installed.Served = freed, retired > 0
	if err != nil {
		installed.Warning = "the download succeeded, but cleaning up the previous files failed: " + err.Error()
	}
	return installed, nil
}

// presentFiles reports which selected files are already in place and
// valid: recorded in the manifest with the same digest and size, or
// checked against their digest.
func (store Store) presentFiles(manifest []Installed, installed Installed, progress Progress) map[string]bool {
	recorded := map[string]string{}
	for _, entry := range manifest {
		for _, file := range entry.Files {
			recorded[store.FilePath(entry, file)] = file.SHA256
		}
	}
	present := map[string]bool{}
	for _, file := range installed.Files {
		path := store.FilePath(installed, file)
		info, err := os.Stat(path)
		if err != nil || info.Size() != file.Size {
			continue
		}
		if recorded[path] == file.SHA256 {
			present[file.Path] = true
			continue
		}
		if progress != nil {
			progress(file.Path, 0, file.Size, true)
		}
		if sum, err := fileDigest(path); err == nil && sum == file.SHA256 {
			present[file.Path] = true
		}
	}
	return present
}

// release deals with the files of replaced entries that no installed entry
// lists any more: files a model service still loads are recorded for
// clean to delete later, the rest are deleted now. It returns the bytes
// freed and how many files it kept for services. The caller holds both
// locks.
func (store Store) release(manifest, replaced []Installed) (int64, int, error) {
	installed := store.manifestPaths(manifest)
	services, err := store.Services()
	if err != nil {
		return 0, 0, err
	}
	loaded := loadedPaths(services)
	retired, err := store.retired()
	if err != nil {
		return 0, 0, err
	}
	retired = slices.DeleteFunc(retired, func(path string) bool { return installed[path] })
	var freed int64
	kept := 0
	for _, entry := range replaced {
		for _, file := range entry.Files {
			path := store.FilePath(entry, file)
			switch {
			case installed[path]:
			case loaded[path]:
				if !slices.Contains(retired, path) {
					retired = append(retired, path)
					kept++
				}
			default:
				if os.Remove(path) == nil {
					freed += file.Size
					store.removeEmptyParents(filepath.Dir(path))
				}
			}
		}
	}
	return freed, kept, store.writeRetired(retired)
}

func loadedPaths(services []Service) map[string]bool {
	loaded := map[string]bool{}
	for _, service := range services {
		for _, path := range service.Files {
			loaded[path] = true
		}
	}
	return loaded
}

func (store Store) manifestPaths(manifest []Installed) map[string]bool {
	paths := map[string]bool{}
	for _, entry := range manifest {
		for _, file := range entry.Files {
			paths[store.FilePath(entry, file)] = true
		}
	}
	return paths
}

func (store Store) writeRetired(paths []string) error {
	if paths == nil {
		paths = []string{}
	}
	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(store.Dir, "retired.json"), data)
}

func (store Store) retired() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(store.Dir, "retired.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	return paths, json.Unmarshal(data, &paths)
}

// checkSpace refuses a download that would leave less than freeMargin.
func (store Store) checkSpace(installed Installed, present map[string]bool) error {
	var needed int64
	for _, file := range installed.Files {
		if present[file.Path] {
			continue
		}
		needed += file.Size
		if info, err := os.Stat(store.partialPath(installed, file)); err == nil {
			needed -= min(info.Size(), file.Size)
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

// removeStalePartials deletes unfinished downloads of file with another
// digest. Names are matched literally, since repository paths may contain
// glob characters.
func (store Store) removeStalePartials(installed Installed, file File) {
	destination := store.FilePath(installed, file)
	keep := store.partialPath(installed, file)
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil {
		return
	}
	prefix := filepath.Base(destination) + "."
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(filepath.Dir(destination), name)
		if entry.Type().IsRegular() && path != keep && len(name) == len(prefix)+16+len(partialSuffix) && strings.HasPrefix(name, prefix) && ownPartial(name) {
			_ = os.Remove(path)
		}
	}
}

// ownPartial reports whether name is an unfinished download overload
// named: <file>.<16 hex digits of its digest>.part.
func ownPartial(name string) bool {
	stem, ok := strings.CutSuffix(name, partialSuffix)
	if !ok || len(stem) < 18 || stem[len(stem)-17] != '.' {
		return false
	}
	_, err := hex.DecodeString(stem[len(stem)-16:])
	return err == nil
}

// FilePath is where one of an installed entry's files is stored: each
// repository revision has its own directory.
func (store Store) FilePath(installed Installed, file File) string {
	return filepath.Join(store.Dir, filepath.FromSlash(installed.Repository), installed.Revision, filepath.FromSlash(file.Path))
}

// partialPath names an unfinished download after the digest it should
// end with.
func (store Store) partialPath(installed Installed, file File) string {
	return store.FilePath(installed, file) + "." + file.SHA256[:16] + partialSuffix
}

// download fetches and verifies one file into its partial file and
// returns that file's path, for Pull to move into place.
func (store Store) download(ctx context.Context, hub Hub, installed Installed, file File, progress Progress) (string, error) {
	if !safeRelative(file.Path) || !digestPattern.MatchString(file.SHA256) || !revisionPattern.MatchString(installed.Revision) {
		return "", errors.New("invalid file entry")
	}
	partial := store.partialPath(installed, file)
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		return "", err
	}
	if info, err := os.Stat(partial); err == nil && info.Size() > 0 && progress != nil {
		progress(file.Path, info.Size(), file.Size, true)
	}
	output, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return "", err
	}
	defer func() { _ = output.Close() }()
	digest := sha256.New()
	offset, err := io.Copy(digest, output)
	if err != nil {
		return "", err
	}
	if offset > file.Size {
		if err := restart(output, digest); err != nil {
			return "", err
		}
		offset = 0
	}
	if offset < file.Size {
		offset, err = fetch(ctx, hub, hub.fileURL(installed.Repository, installed.Revision, file.Path), file, output, digest, offset, progress)
		if err != nil {
			return "", err
		}
	}
	if offset != file.Size {
		return "", fmt.Errorf("download stopped at %d of %d bytes; run the command again to resume", offset, file.Size)
	}
	if hex.EncodeToString(digest.Sum(nil)) != file.SHA256 {
		_ = output.Close()
		_ = os.Remove(partial)
		return "", errors.New("downloaded file does not match its published SHA-256; the partial file was removed")
	}
	if err := output.Sync(); err != nil {
		return "", err
	}
	return partial, output.Close()
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
// installed entry still uses or a model service still loads. A model that
// is being served cannot be removed.
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
	if err := store.writeManifest(manifest); err != nil {
		return Installed{}, err
	}
	freed, _, err := store.release(manifest, []Installed{installed})
	installed.Freed = freed
	return installed, err
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

// Clean removes partial downloads, and weights a newer download replaced
// once no model service loads them, and returns the bytes freed. Other
// files in the directory are left alone.
func (store Store) Clean() (int64, error) {
	unlock, err := store.lock(".lock")
	if err != nil {
		return 0, err
	}
	defer unlock()
	unlockServices, err := store.lock(".services.lock")
	if err != nil {
		return 0, err
	}
	defer unlockServices()
	manifest, err := store.readManifest()
	if err != nil {
		return 0, err
	}
	installed := store.manifestPaths(manifest)
	services, err := store.Services()
	if err != nil {
		return 0, err
	}
	loaded := loadedPaths(services)
	retired, err := store.retired()
	if err != nil {
		return 0, err
	}
	var freed int64
	var still []string
	for _, path := range retired {
		if installed[path] {
			continue
		}
		if loaded[path] {
			still = append(still, path)
			continue
		}
		if info, err := os.Stat(path); err == nil {
			if err := os.Remove(path); err != nil {
				return freed, err
			}
			freed += info.Size()
			store.removeEmptyParents(filepath.Dir(path))
		}
	}
	if err := store.writeRetired(still); err != nil {
		return freed, err
	}
	err = filepath.WalkDir(store.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.Type().IsRegular() && ownPartial(entry.Name()) {
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
