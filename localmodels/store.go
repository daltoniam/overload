package localmodels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/daltoniam/overload/catalog"
)

// Store keeps downloaded weights under Dir, one directory per repository,
// and a manifest of what finished downloading and verified.
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
	ID           string    `json:"id"`
	Quant        string    `json:"quant"`
	Repository   string    `json:"repository"`
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

// Progress reports bytes downloaded so far out of total for one file.
type Progress func(path string, done, total int64)

const partialSuffix = ".part"

// Pull downloads quant of the catalog model from repository (the entry's
// repository for that quant when empty), resuming partial files and
// verifying each against its published SHA-256. Files already verified
// are not downloaded again.
func (store Store) Pull(ctx context.Context, hub Hub, model catalog.Model, quant, repository string, progress Progress) (Installed, error) {
	if repository == "" {
		for _, weights := range model.Weights {
			if strings.EqualFold(weights.Quant, quant) && weights.Repository != "" {
				repository, quant = weights.Repository, weights.Quant
			}
		}
	}
	if repository == "" {
		return Installed{}, fmt.Errorf("the catalog has no download for %s %s; pass a Hugging Face repository", model.ID, quant)
	}
	files, err := hub.ListFiles(ctx, repository)
	if err != nil {
		return Installed{}, err
	}
	selected, err := SelectQuant(files, quant)
	if err != nil {
		return Installed{}, fmt.Errorf("%s: %w", repository, err)
	}
	unlock, err := store.lock()
	if err != nil {
		return Installed{}, err
	}
	defer unlock()
	for _, file := range selected {
		if err := store.download(ctx, hub, repository, file, progress); err != nil {
			return Installed{}, fmt.Errorf("%s: %w", file.Path, err)
		}
	}
	installed := Installed{ID: model.ID, Quant: quant, Repository: repository, Files: selected, DownloadedAt: store.now().UTC()}
	manifest, err := store.readManifest()
	if err != nil {
		return Installed{}, err
	}
	manifest = slices.DeleteFunc(manifest, func(entry Installed) bool { return entry.ID == installed.ID && strings.EqualFold(entry.Quant, quant) })
	manifest = append(manifest, installed)
	return installed, store.writeManifest(manifest)
}

// Path is where a repository file is stored.
func (store Store) Path(repository, path string) string {
	return filepath.Join(store.Dir, filepath.FromSlash(repository), filepath.FromSlash(path))
}

func (store Store) download(ctx context.Context, hub Hub, repository string, file File, progress Progress) error {
	if !safeRelative(file.Path) || !digestPattern.MatchString(file.SHA256) {
		return errors.New("invalid file entry")
	}
	destination := store.Path(repository, file.Path)
	if info, err := os.Stat(destination); err == nil && info.Size() == file.Size {
		if sum, err := fileDigest(destination); err == nil && sum == file.SHA256 {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	partial := destination + partialSuffix
	output, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = output.Close() }()
	hash := sha256.New()
	offset, err := io.Copy(hash, output)
	if err != nil {
		return err
	}
	if offset > file.Size {
		if err := restart(output, hash); err != nil {
			return err
		}
		offset = 0
	}
	if offset < file.Size {
		if offset, err = fetch(ctx, hub, repository, file, output, hash, offset, progress); err != nil {
			return err
		}
	}
	if offset != file.Size || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
		_ = output.Close()
		_ = os.Remove(partial)
		return errors.New("downloaded file does not match its published size and SHA-256; the partial file was removed")
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(partial, destination)
}

// fetch downloads file from offset (or from the start when the server
// ignores the range) into output and hash, and returns the bytes written.
func fetch(ctx context.Context, hub Hub, repository string, file File, output *os.File, hash io.Writer, offset int64, progress Progress) (int64, error) {
	request, err := hub.request(ctx, hub.fileURL(repository, file.Path))
	if err != nil {
		return offset, err
	}
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	response, err := hub.Client.Do(request)
	if err != nil {
		return offset, err
	}
	defer func() { _ = response.Body.Close() }()
	switch {
	case response.StatusCode == http.StatusPartialContent && offset > 0 && strings.HasPrefix(response.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", offset)):
	case response.StatusCode == http.StatusOK:
		if err := restart(output, hash); err != nil {
			return offset, err
		}
		offset = 0
	default:
		return offset, errors.New(statusMessage(response))
	}
	reader := io.LimitReader(response.Body, file.Size-offset+1)
	buffer := make([]byte, 1<<20)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if _, err := output.Write(buffer[:n]); err != nil {
				return offset, err
			}
			_, _ = hash.Write(buffer[:n])
			offset += int64(n)
			if progress != nil {
				progress(file.Path, offset, file.Size)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return offset, nil
		}
		if readErr != nil {
			return offset, fmt.Errorf("download interrupted at %d of %d bytes (run the command again to resume): %w", offset, file.Size, readErr)
		}
	}
}

func restart(output *os.File, hash io.Writer) error {
	if err := output.Truncate(0); err != nil {
		return err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if resettable, ok := hash.(interface{ Reset() }); ok {
		resettable.Reset()
	}
	return nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
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

// Remove deletes an installed quantization's files.
func (store Store) Remove(id, quant string) (Installed, error) {
	unlock, err := store.lock()
	if err != nil {
		return Installed{}, err
	}
	defer unlock()
	installed, err := store.Find(id, quant)
	if err != nil {
		return Installed{}, err
	}
	for _, file := range installed.Files {
		if err := os.Remove(store.Path(installed.Repository, file.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Installed{}, err
		}
	}
	manifest, err := store.readManifest()
	if err != nil {
		return Installed{}, err
	}
	manifest = slices.DeleteFunc(manifest, func(entry Installed) bool {
		return entry.ID == installed.ID && entry.Quant == installed.Quant
	})
	return installed, store.writeManifest(manifest)
}

// Clean removes partial downloads and returns the bytes freed.
func (store Store) Clean() (int64, error) {
	unlock, err := store.lock()
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
	var stat syscall.Statfs_t
	if err := syscall.Statfs(existingParent(store.Dir), &stat); err != nil {
		return used, 0, err
	}
	return used, int64(stat.Bavail) * int64(stat.Bsize), nil
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
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// lock serializes changes to the store across processes, so two pulls do
// not write the same partial file.
func (store Store) lock() (func(), error) {
	if err := os.MkdirAll(store.Dir, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(store.Dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("another overload models command is using the model directory")
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
