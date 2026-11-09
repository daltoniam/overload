package localmodels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/launchd"
)

const testRevision = "0123456789abcdef0123456789abcdef01234567"

// fakeHub serves a Hugging Face repository named test/flint-GGUF.
type fakeHub struct {
	mu        sync.Mutex
	files     map[string][]byte
	digests   map[string]string
	pageSize  int
	failAfter atomic.Int64
	short     atomic.Bool
	stall     atomic.Bool
	ranges    atomic.Int64
	noRange   bool
	badRange  atomic.Bool
	redirect  string
	revision  string
	// status, when set, answers every file request with it.
	status int
}

func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func (hub *fakeHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		revision := hub.revision
		if revision == "" {
			revision = testRevision
		}
		switch r.URL.Path {
		case "/api/models/test/flint-GGUF/revision/main":
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": revision})
			return
		case "/api/models/test/flint-GGUF/tree/" + revision:
			var entries []map[string]any
			for path, content := range hub.files {
				digest := hub.digests[path]
				if digest == "" {
					digest = digestOf(content)
				}
				entries = append(entries, map[string]any{"type": "file", "path": path, "lfs": map[string]any{"oid": digest, "size": len(content)}})
			}
			entries = append(entries, map[string]any{"type": "file", "path": "README.md"}, map[string]any{"type": "directory", "path": "Q8_0"}, map[string]any{"type": "file", "path": "../evil-Q4_K_M.gguf", "lfs": map[string]any{"oid": strings.Repeat("a", 64), "size": 1}})
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if hub.pageSize > 0 {
				start := page * hub.pageSize
				end := min(start+hub.pageSize, len(entries))
				if end < len(entries) {
					w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?recursive=true&page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
				}
				entries = entries[min(start, len(entries)):end]
			}
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		prefix := "/test/flint-GGUF/resolve/" + revision + "/"
		path, ok := strings.CutPrefix(r.URL.Path, prefix)
		content, found := hub.files[path]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		if hub.status != 0 {
			w.WriteHeader(hub.status)
			return
		}
		if hub.redirect != "" && r.URL.Query().Get("signed") == "" {
			http.Redirect(w, r, hub.redirect+r.URL.Path+"?signed=1", http.StatusFound)
			return
		}
		start := 0
		if header := r.Header.Get("Range"); header != "" && !hub.noRange {
			hub.ranges.Add(1)
			value, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(header, "bytes="), "-"))
			if hub.badRange.Swap(false) || value >= len(content) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start = value
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
		}
		body := content[start:]
		if hub.short.Swap(false) {
			_, _ = w.Write(body[:len(body)/2])
			return
		}
		if hub.stall.Swap(false) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body[:10])
			w.(http.Flusher).Flush()
			hub.mu.Unlock()
			<-r.Context().Done()
			hub.mu.Lock()
			return
		}
		if limit := hub.failAfter.Swap(0); limit > 0 {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body[:limit])
			w.(http.Flusher).Flush()
			if hijacker, ok := w.(http.Hijacker); ok {
				conn, _, _ := hijacker.Hijack()
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write(body)
	})
}

func testModel() catalog.Model {
	model, _ := catalog.Lookup("recursive-flint")
	model.Weights = []catalog.Weights{{Quant: "Q4_K_M", Repository: "test/flint-GGUF", Format: "gguf"}}
	return model
}

func newHub(t *testing.T, files map[string][]byte) (*fakeHub, Hub) {
	fake := &fakeHub{files: files, digests: map[string]string{}}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	return fake, Hub{BaseURL: server.URL, Client: server.Client(), Stall: 200 * time.Millisecond}
}

func TestPullResumesAndVerifies(t *testing.T) {
	content := []byte(strings.Repeat("flint weights ", 5000))
	fake, hub := newHub(t, map[string][]byte{"flint-Q4_K_M.gguf": content, "flint-Q8_0.gguf": []byte("other")})
	fake.pageSize = 2
	store := NewStore(t.TempDir())
	ctx := context.Background()
	file := File{Path: "flint-Q4_K_M.gguf", Size: int64(len(content)), SHA256: digestOf(content)}

	fake.failAfter.Store(1000)
	if _, err := store.Pull(ctx, hub, testModel(), "q4_k_m", "", nil); err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("interrupted pull: %v", err)
	}
	partial, err := os.Stat(store.partialPath("test/flint-GGUF", file))
	if err != nil || partial.Size() != 1000 {
		t.Fatalf("partial file: %v %v", partial, err)
	}
	fake.short.Store(true)
	if _, err := store.Pull(ctx, hub, testModel(), "q4_k_m", "", nil); err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("short response: %v", err)
	}
	if partial, err := os.Stat(store.partialPath("test/flint-GGUF", file)); err != nil || partial.Size() <= 1000 {
		t.Fatalf("a short response discarded the partial file: %v", err)
	}
	fake.stall.Store(true)
	if _, err := store.Pull(ctx, hub, testModel(), "q4_k_m", "", nil); err == nil {
		t.Fatal("a stalled download did not fail")
	}
	fake.badRange.Store(true)
	var last int64
	installed, err := store.Pull(ctx, hub, testModel(), "q4_k_m", "", func(_ string, done, _ int64, _ bool) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if last != int64(len(content)) || installed.Quant != "Q4_K_M" || installed.Revision != testRevision || installed.Size() != int64(len(content)) {
		t.Fatalf("installed %+v, last %d", installed, last)
	}
	data, err := os.ReadFile(store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf"))
	if err != nil || string(data) != string(content) {
		t.Fatal("downloaded file differs")
	}
	ranges := fake.ranges.Load()
	verified := false
	if _, err := store.Pull(ctx, hub, testModel(), "Q4_K_M", "", func(_ string, _, _ int64, verifying bool) { verified = verifying }); err != nil || fake.ranges.Load() != ranges || !verified {
		t.Fatalf("second pull downloaded again: %v", err)
	}

	fake.digests["flint-Q8_0.gguf"] = strings.Repeat("0", 64)
	if _, err := store.Pull(ctx, hub, testModel(), "Q8_0", "test/flint-GGUF", nil); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("corrupt pull: %v", err)
	}
	if _, err := store.Pull(ctx, hub, testModel(), "Q6_K", "", nil); err == nil || !strings.Contains(err.Error(), "no download") {
		t.Fatalf("missing quant: %v", err)
	}
	if _, err := store.Pull(ctx, hub, testModel(), "Q4_K_M", "../x", nil); err == nil {
		t.Fatal("pulled from an invalid repository")
	}

	used, free, err := store.Usage()
	if err != nil || used < int64(len(content)) || free <= 0 {
		t.Fatalf("usage = %d, %d, %v", used, free, err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "stale.gguf.0123456789abcdef.part"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if freed, err := store.Clean(); err != nil || freed < 5 {
		t.Fatalf("clean = %d, %v", freed, err)
	}
	if _, err := store.Remove("recursive-flint", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "test")); !os.IsNotExist(err) {
		t.Fatal("removing the model left its directories")
	}
}

func TestFailedUpdateKeepsTheInstalledRevision(t *testing.T) {
	first, second := []byte(strings.Repeat("1", 4096)), []byte(strings.Repeat("2", 4096))
	fake, hub := newHub(t, map[string][]byte{"flint-Q4_K_M-00001-of-00002.gguf": first, "flint-Q4_K_M-00002-of-00002.gguf": second})
	store := NewStore(t.TempDir())
	if _, err := store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.files["flint-Q4_K_M-00001-of-00002.gguf"] = []byte(strings.Repeat("3", 4096))
	fake.digests["flint-Q4_K_M-00002-of-00002.gguf"] = strings.Repeat("0", 64)
	fake.files["flint-Q4_K_M-00002-of-00002.gguf"] = []byte(strings.Repeat("4", 4096))
	fake.revision = strings.Repeat("e", 40)
	fake.mu.Unlock()
	if _, err := store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil); err == nil {
		t.Fatal("an update with a corrupt part succeeded")
	}
	if data, _ := os.ReadFile(store.Path("test/flint-GGUF", "flint-Q4_K_M-00001-of-00002.gguf")); string(data) != string(first) {
		t.Fatal("a failed update replaced an installed part")
	}
	if installed, err := store.Find(testModel().ID, ""); err != nil || installed.Revision != testRevision {
		t.Fatalf("installed = %+v, %v", installed, err)
	}
}

func TestPullFromAnotherRepositoryRemovesTheOldFiles(t *testing.T) {
	content := []byte(strings.Repeat("w", 4096))
	_, hub := newHub(t, map[string][]byte{"flint-Q4_K_M.gguf": content})
	store := NewStore(t.TempDir())
	old := Installed{ID: testModel().ID, Quant: "Q4_K_M", Repository: "old/flint-GGUF", Files: []File{{Path: "flint-Q4_K_M.gguf", Size: 3}}}
	oldPath := store.Path(old.Repository, old.Files[0].Path)
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.writeManifest([]Installed{old}); err != nil {
		t.Fatal(err)
	}
	if err := store.writeServices([]Service{{ID: old.ID, Quant: old.Quant, Port: 8080, Files: []string{oldPath}}}); err != nil {
		t.Fatal(err)
	}
	installed, err := store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil)
	if err != nil || !installed.Served || installed.Freed != 0 {
		t.Fatalf("pull while served = %+v, %v", installed, err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatal("a served model's files were deleted")
	}
	if _, err := store.Clean(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatal("clean deleted files a running service loads")
	}
	if err := store.writeServices(nil); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(store.Dir, "mine", "notes.gguf")
	if err := os.MkdirAll(filepath.Dir(unrelated), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if freed, err := store.Clean(); err != nil || freed != 3 {
		t.Fatalf("clean of retired weights = %d, %v", freed, err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("clean deleted a GGUF overload did not write")
	}
	foreign := filepath.Join(store.Dir, "mine", "video.mp4.part")
	if err := os.WriteFile(foreign, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Clean(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("clean deleted another program's partial download")
	}
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.writeServices(nil); err != nil {
		t.Fatal(err)
	}
	if err := store.writeManifest([]Installed{old}); err != nil {
		t.Fatal(err)
	}
	installed, err = store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil)
	if err != nil || installed.Freed != 3 {
		t.Fatalf("pull = %+v, %v", installed, err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "old")); !os.IsNotExist(err) {
		t.Fatal("the replaced repository's files were orphaned")
	}
}

func TestCleanKeepsRetiredFilesThatAreInstalledAgain(t *testing.T) {
	store := NewStore(t.TempDir())
	entry := Installed{ID: "recursive-flint", Quant: "Q4_K_M", Repository: "a/flint", Files: []File{{Path: "flint-Q4_K_M.gguf", Size: 1}}}
	path := store.Path(entry.Repository, entry.Files[0].Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.writeManifest([]Installed{entry}); err != nil {
		t.Fatal(err)
	}
	if err := store.writeRetired([]string{path}); err != nil {
		t.Fatal(err)
	}
	if freed, err := store.Clean(); err != nil || freed != 0 {
		t.Fatalf("clean = %d, %v", freed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("clean deleted an installed file")
	}
	if retired, _ := store.retired(); len(retired) != 0 {
		t.Fatalf("an installed file stayed retired: %v", retired)
	}
}

func TestPullForgetsRetiredFilesItInstallsAgain(t *testing.T) {
	content := []byte(strings.Repeat("r", 4096))
	_, hub := newHub(t, map[string][]byte{"flint-Q4_K_M.gguf": content})
	store := NewStore(t.TempDir())
	path := store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf")
	if err := store.writeRetired([]string{path}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil); err != nil {
		t.Fatal(err)
	}
	if retired, _ := store.retired(); len(retired) != 0 {
		t.Fatalf("a re-installed file stayed retired: %v", retired)
	}
}

func TestOwnPartial(t *testing.T) {
	for name, want := range map[string]bool{"a.gguf.0123456789abcdef.part": true, "a.gguf.mine.part": false, "video.mp4.part": false, "a.gguf.0123456789abcdeg.part": false, ".0123456789abcdef.part": false} {
		if ownPartial(name) != want {
			t.Errorf("%s: %v", name, !want)
		}
	}
}

func TestPaginationStaysOnTheHub(t *testing.T) {
	hub := Hub{BaseURL: "https://huggingface.co"}
	for link, want := range map[string]string{
		`</api/models/a/b/tree/x?cursor=2>; rel="next"`:                       "/api/models/a/b/tree/x?cursor=2",
		`<https://huggingface.co/api/models/a/b/tree/x?cursor=3>; rel="next"`: "/api/models/a/b/tree/x?cursor=3",
		`<https://huggingface.co/api/x>; rel="prev"`:                          "",
	} {
		if got, err := hub.nextPage(link); err != nil || got != want {
			t.Errorf("%s: %q %v", link, got, err)
		}
	}
	for _, link := range []string{`<.attacker.example/page>; rel="next"`, `<https://attacker.example/api/x>; rel="next"`, `<//attacker.example/api/x>; rel="next"`, `<http://huggingface.co/api/x>; rel="next"`, `</models/a>; rel="next"`} {
		if _, err := hub.nextPage(link); err == nil {
			t.Errorf("%s accepted", link)
		}
	}
}

func TestPartialFromAnotherRevisionIsNotResumed(t *testing.T) {
	old, current := []byte(strings.Repeat("a", 4096)), []byte(strings.Repeat("b", 4096))
	fake, hub := newHub(t, map[string][]byte{"flint-Q4_K_M.gguf": old})
	store := NewStore(t.TempDir())
	fake.failAfter.Store(100)
	_, _ = store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil)
	fake.mu.Lock()
	fake.files["flint-Q4_K_M.gguf"], fake.revision = current, strings.Repeat("f", 40)
	fake.mu.Unlock()
	if _, err := store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil); err != nil {
		t.Fatalf("pull after the repository changed: %v", err)
	}
	if data, _ := os.ReadFile(store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf")); string(data) != string(current) {
		t.Fatal("mixed revisions")
	}
}

func TestPullFailsOnUnexpectedStatus(t *testing.T) {
	content := []byte(strings.Repeat("x", 4096))
	fake, hub := newHub(t, map[string][]byte{"flint-Q4_K_M.gguf": content})
	store := NewStore(t.TempDir())
	destination := store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := destination + ".00000000deadbeef" + partialSuffix
	odd := store.Path("test/flint-GGUF", "[odd].gguf") + ".00000000deadbeef" + partialSuffix
	if err := os.WriteFile(odd, []byte("other file"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(odd); err != nil {
			t.Error("a partial file of another file was removed")
		}
	})
	if err := os.WriteFile(stale, []byte("old revision"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusRequestedRangeNotSatisfiable, http.StatusPartialContent} {
		fake.mu.Lock()
		fake.status = status
		fake.mu.Unlock()
		done := make(chan error, 1)
		go func() {
			_, err := store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("status %d on a fresh download succeeded", status)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("status %d on a fresh download looped", status)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a partial file from another revision was kept")
	}
}

func TestPullRestartsWithoutRangeSupport(t *testing.T) {
	content := []byte(strings.Repeat("x", 4096))
	fake, hub := newHub(t, map[string][]byte{"flint-Q4_K_M.gguf": content})
	fake.noRange = true
	store := NewStore(t.TempDir())
	partial := store.partialPath("test/flint-GGUF", File{Path: "flint-Q4_K_M.gguf", SHA256: digestOf(content)})
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestRedirectsDropTheToken(t *testing.T) {
	content := []byte("weights")
	var gotAuth atomic.Value
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write(content)
	}))
	defer cdn.Close()
	fake, hub := newHub(t, map[string][]byte{"flint-Q4_K_M.gguf": content})
	fake.redirect = strings.Replace(cdn.URL, "127.0.0.1", "localhost", 1)
	hub.Token = "hf_secret"
	store := NewStore(t.TempDir())
	if _, err := store.Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil); err != nil {
		t.Fatal(err)
	}
	if gotAuth.Load() != "" {
		t.Fatalf("the token went to another host: %v", gotAuth.Load())
	}
}

func TestSelectQuant(t *testing.T) {
	files := []File{{Path: "flint-Q4_K_M.gguf"}, {Path: "flint-Q4_K_M_L.gguf"}, {Path: "Q8_0/flint-Q8_0-00002-of-00002.gguf"}, {Path: "Q8_0/flint-Q8_0-00001-of-00002.gguf"}, {Path: "mmproj-Q8_0.gguf"}, {Path: "flint-IQ3_XXS.gguf"}, {Path: "a-Q6_K.gguf"}, {Path: "b-Q6_K.gguf"}, {Path: "x-Q5_K-00001-of-00003.gguf"}, {Path: "x-Q5_K-00002-of-00003.gguf"}}
	if _, err := SelectQuant([]File{{Path: "flint-IQ4_XS.gguf"}}, "XS"); err == nil {
		t.Error("a quantization matched the tail of another")
	}
	if _, err := SelectQuant([]File{{Path: "model-Q4_K_M-00001-of-00002.gguf"}}, "Q4_K_M"); err == nil {
		t.Error("a lone part of a split model was selected")
	}
	if selected, err := SelectQuant([]File{{Path: "model-Q4_K_M-00001-of-00001.gguf"}}, "Q4_K_M"); err != nil || len(selected) != 1 {
		t.Errorf("a one-part split model: %v", err)
	}
	if selected, err := SelectQuant([]File{{Path: "model_Q4_K_M.gguf"}}, "Q4_K_M"); err != nil || len(selected) != 1 {
		t.Errorf("an underscore-separated name: %v", err)
	}
	for quant, want := range map[string]string{"Q4_K_M": "flint-Q4_K_M.gguf", "q8_0": "Q8_0/flint-Q8_0-00001-of-00002.gguf,Q8_0/flint-Q8_0-00002-of-00002.gguf", "IQ3_XXS": "flint-IQ3_XXS.gguf"} {
		selected, err := SelectQuant(files, quant)
		var paths []string
		for _, file := range selected {
			paths = append(paths, file.Path)
		}
		if err != nil || strings.Join(paths, ",") != want {
			t.Errorf("%s: %v, %v", quant, paths, err)
		}
	}
	for _, quant := range []string{"", "Q2_K", "Q6_K", "Q5_K", "../x", "Q4 K", "M", "XL", "K_M"} {
		if _, err := SelectQuant(files, quant); err == nil {
			t.Errorf("%q selected", quant)
		}
	}
}

func TestListFilesRejectsBadInput(t *testing.T) {
	for _, repository := range []string{"", "a", "../x/y", "a/b/c", "a/.."} {
		if _, err := (Hub{}).ListFiles(context.Background(), repository, testRevision); err == nil {
			t.Errorf("%q accepted", repository)
		}
	}
	if _, err := (Hub{}).ListFiles(context.Background(), "a/b", "main"); err == nil {
		t.Error("a branch name was accepted as a revision")
	}
	for _, path := range []string{"a//b.gguf", "./a.gguf", "a/../b.gguf", "/a.gguf", "a\\b.gguf"} {
		if safeRelative(path) {
			t.Errorf("%q accepted", path)
		}
	}
}

func TestLlamaVersion(t *testing.T) {
	for output, want := range map[string]LlamaVersion{
		"ggml_metal_init: ...\nversion: 11803 (a1b2c3d)\nbuilt with clang": {Build: 11803, Commit: "a1b2c3d"},
		"version: 12058 (unknown)":   {Build: 12058, Commit: "unknown"},
		"version: 0 (a1b2c3d-dirty)": {Build: 0, Commit: "a1b2c3d-dirty"},
	} {
		if got, err := ParseLlamaVersion(output); err != nil || got != want {
			t.Errorf("%q: %+v %v", output, got, err)
		}
	}
	if _, err := ParseLlamaVersion("llama-server 1.0"); err == nil {
		t.Fatal("parsed nonsense")
	}
	flint, _ := catalog.Lookup("recursive-flint")
	if err := CheckRuntime(flint, LlamaVersion{Build: 11669}); err == nil || !strings.Contains(err.Error(), "b11801 or newer") {
		t.Fatalf("old build: %v", err)
	}
	if err := CheckRuntime(flint, LlamaVersion{Build: 0}); err == nil || !strings.Contains(err.Error(), "--skip-version-check") {
		t.Fatalf("unknown build: %v", err)
	}
	if err := CheckRuntime(flint, LlamaVersion{Build: 11801}); err != nil {
		t.Fatal(err)
	}
	ripple, _ := catalog.Lookup("ripple-65b-preview")
	if err := CheckRuntime(ripple, LlamaVersion{Build: 0}); err != nil {
		t.Fatalf("an entry without a minimum build: %v", err)
	}
	qwen, _ := catalog.Lookup("qwen3.8-flash-next")
	if err := CheckRuntime(qwen, LlamaVersion{Build: 99999}); err == nil {
		t.Fatal("ds4 model accepted for llama.cpp")
	}
	script := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'version: 12058 (deadbee)' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if version, err := DetectLlama(context.Background(), script); err != nil || version.Build != 12058 {
		t.Fatalf("detect = %+v, %v", version, err)
	}
}

type fakeManager struct {
	written  map[string]launchd.Agent
	started  []string
	stopped  []string
	removed  []string
	startErr error
	writeErr error
}

func (manager *fakeManager) Write(agent launchd.Agent) error {
	if manager.writeErr != nil {
		return manager.writeErr
	}
	manager.written[agent.Label] = agent
	return nil
}
func (manager *fakeManager) Start(label string) error {
	manager.started = append(manager.started, label)
	return manager.startErr
}
func (manager *fakeManager) Stop(label string) { manager.stopped = append(manager.stopped, label) }
func (manager *fakeManager) Remove(label string) error {
	manager.removed = append(manager.removed, label)
	return nil
}
func (manager *fakeManager) State(string) string { return "running" }

func TestServices(t *testing.T) {
	busy := map[int]bool{9000: true}
	probe := PortInUse
	PortInUse = func(port int) bool { return busy[port] }
	t.Cleanup(func() { PortInUse = probe })
	store := NewStore(t.TempDir())
	model := testModel()
	installed := Installed{ID: model.ID, Quant: "Q4_K_M", Repository: "test/flint-GGUF", Files: []File{{Path: "flint-Q4_K_M.gguf"}}}
	options := ServeOptions{Binary: "/opt/llama/llama-server", LabelPrefix: "dev.overload", Port: 8080, Parallel: 2, LogDir: "/tmp/logs"}
	agent, service, err := store.ServiceAgent(model, installed, options)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(agent.Args, " ")
	for _, want := range []string{"/opt/llama/llama-server -m " + store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf"), "--host 127.0.0.1 --port 8080 --alias flint", "-c 524288", "--reasoning-format deepseek", "-np 2"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	options.Parallel = 1
	if single, _, _ := store.ServiceAgent(model, installed, options); !strings.Contains(strings.Join(single.Args, " "), "-np 1") {
		t.Error("a single slot is not set explicitly")
	}
	if service.Label != "dev.overload.model.recursive-flint" || service.BaseURL() != "http://127.0.0.1:8080/v1" || service.ModelName != "flint" {
		t.Fatalf("service = %+v", service)
	}
	for _, bad := range []ServeOptions{{Binary: "/b", LogDir: "/l", Port: 80, Parallel: 1}, {Binary: "/b", LogDir: "/l", Port: 8080, Parallel: 0}, {Binary: "llama-server", LogDir: "/l", Port: 8080, Parallel: 1}} {
		if _, _, err := store.ServiceAgent(model, installed, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if _, _, err := NewStore("models").ServiceAgent(model, installed, ServeOptions{Binary: "/b", LogDir: "/l", Port: 8080, Parallel: 1}); err == nil {
		t.Error("a relative model directory was accepted")
	}
	weights := store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(weights), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(weights, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.writeManifest([]Installed{installed}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{written: map[string]launchd.Agent{}}
	if err := store.Serve(manager, launchd.Agent{Label: "dev.overload.model.recursive-flint"}, Service{ID: model.ID, Quant: "Q4_K_M", Label: "dev.overload.model.recursive-flint", Port: 9000}); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("busy port: %v", err)
	}
	if err := store.Serve(manager, agent, service); err != nil {
		t.Fatal(err)
	}
	busy[8080] = true
	if err := store.Serve(manager, agent, service); err != nil {
		t.Fatalf("restarting a model's own service: %v", err)
	}
	manager.stopped = nil
	other := Service{ID: "other", Label: "dev.overload.model.other", Port: 8080}
	if err := store.Serve(manager, launchd.Agent{Label: other.Label}, other); err == nil {
		t.Fatal("two services on one port")
	}
	if len(manager.stopped) != 0 {
		t.Fatalf("a refused serve stopped %v", manager.stopped)
	}
	if err := store.Serve(manager, launchd.Agent{Label: "dev.overload.model.missing"}, Service{ID: "missing", Port: 8095}); err == nil || !strings.Contains(err.Error(), "not downloaded") {
		t.Fatalf("served a model that is not downloaded: %v", err)
	}
	if _, err := store.Remove(model.ID, ""); err == nil || !strings.Contains(err.Error(), "being served") {
		t.Fatalf("removed a served model: %v", err)
	}
	manager.writeErr = errors.New("disk full")
	manager.started = nil
	if err := store.Serve(manager, agent, service); err == nil || len(manager.started) != 1 {
		t.Fatalf("a failed rewrite did not restart the previous service: %v %v", err, manager.started)
	}
	if services, _ := store.Services(); len(services) != 1 || services[0].Port != service.Port {
		t.Fatalf("a failed rewrite changed the service list: %+v", services)
	}
	manager.writeErr = nil
	manager.startErr = errors.New("boom")
	if err := store.Serve(manager, agent, service); err == nil || manager.removed[len(manager.removed)-1] != agent.Label {
		t.Fatalf("a service that failed to start was not removed: %v %v", err, manager.removed)
	}
	if services, err := store.Services(); err != nil || len(services) != 0 {
		t.Fatalf("a failed restart kept its record: %+v %v", services, err)
	}
	if err := store.StopService(manager, "recursive-flint"); err == nil {
		t.Fatal("stopped a missing service")
	}
}

func TestRemoveKeepsSharedFiles(t *testing.T) {
	store := NewStore(t.TempDir())
	path := store.Path("test/shared", "model.gguf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []File{{Path: "model.gguf", Size: 1}}
	if err := store.writeManifest([]Installed{{ID: "a", Quant: "Q8_0", Repository: "test/shared", Files: files}, {ID: "b", Quant: "Q8_0", Repository: "test/shared", Files: files}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remove("a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a file another entry uses was deleted")
	}
}

func TestHealth(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusServiceUnavailable)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(int(status.Load()))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"flint"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(parsed.Port())
	service := Service{ID: "flint", Port: port, ModelName: "flint"}
	ctx := context.Background()
	if health := CheckHealth(ctx, server.Client(), service); health != HealthLoading {
		t.Fatalf("health = %s", health)
	}
	status.Store(http.StatusOK)
	if err := WaitReady(ctx, server.Client(), service, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := CheckModel(ctx, server.Client(), service); err != nil {
		t.Fatal(err)
	}
	if err := CheckModel(ctx, server.Client(), Service{Port: port, ModelName: "qwen"}); err == nil {
		t.Fatal("a different model's server was accepted")
	}
	if err := WaitReady(ctx, server.Client(), Service{Port: 1}, time.Millisecond, 20*time.Millisecond); !errors.Is(err, ErrNotStarting) {
		t.Fatalf("unreachable server: %v", err)
	}
	status.Store(http.StatusServiceUnavailable)
	crashed := make(chan error, 1)
	go func() { crashed <- WaitReady(ctx, server.Client(), service, time.Millisecond, 100*time.Millisecond) }()
	time.Sleep(30 * time.Millisecond)
	server.Close()
	select {
	case err := <-crashed:
		if !errors.Is(err, ErrNotStarting) {
			t.Fatalf("a server that crashed while loading: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waited out the deadline for a server that crashed while loading")
	}
}

func TestHTTPSDownloadsRefuseHTTPRedirects(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("weights")) }))
	defer cdn.Close()
	fake := &fakeHub{files: map[string][]byte{"flint-Q4_K_M.gguf": []byte("weights")}, digests: map[string]string{}, redirect: cdn.URL}
	server := httptest.NewTLSServer(fake.handler())
	defer server.Close()
	hub := Hub{BaseURL: server.URL, Client: server.Client(), Token: "hf_secret"}
	if _, err := NewStore(t.TempDir()).Pull(context.Background(), hub, testModel(), "Q4_K_M", "", nil); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("followed an https to http redirect: %v", err)
	}
}

func TestStalePartialsOnlyMatchDigestNames(t *testing.T) {
	store := NewStore(t.TempDir())
	file := File{Path: "flint-Q4_K_M.gguf", SHA256: strings.Repeat("c", 64)}
	destination := store.Path("test/flint-GGUF", file.Path)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := destination + ".0123456789abcdef" + partialSuffix
	mine := destination + ".mine" + partialSuffix
	sibling := destination + ".foo.gguf.0123456789abcdef" + partialSuffix
	for _, path := range []string{stale, mine, sibling} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store.removeStalePartials("test/flint-GGUF", file)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a partial download from another revision was kept")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("an unrelated .part file was removed")
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatal("a sibling file's partial download was removed")
	}
}
