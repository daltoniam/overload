package localmodels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/launchd"
)

type fakeHub struct {
	files     map[string][]byte
	digests   map[string]string
	failAfter atomic.Int64
	ranges    atomic.Int64
	noRange   bool
}

func (hub *fakeHub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/test/flint-GGUF/tree/main") {
			var entries []map[string]any
			for path, content := range hub.files {
				digest := hub.digests[path]
				if digest == "" {
					sum := sha256.Sum256(content)
					digest = hex.EncodeToString(sum[:])
				}
				entries = append(entries, map[string]any{"type": "file", "path": path, "size": 134, "lfs": map[string]any{"oid": digest, "size": len(content)}})
			}
			entries = append(entries, map[string]any{"type": "file", "path": "README.md", "size": 10}, map[string]any{"type": "directory", "path": "Q8_0"}, map[string]any{"type": "file", "path": "../evil-Q4_K_M.gguf", "lfs": map[string]any{"oid": strings.Repeat("a", 64), "size": 1}})
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		path, ok := strings.CutPrefix(r.URL.Path, "/test/flint-GGUF/resolve/main/")
		content, found := hub.files[path]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		start := 0
		if header := r.Header.Get("Range"); header != "" && !hub.noRange {
			hub.ranges.Add(1)
			value, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(header, "bytes="), "-"))
			start = value
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
		}
		body := content[start:]
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

func TestPullResumesAndVerifies(t *testing.T) {
	content := []byte(strings.Repeat("flint weights ", 5000))
	hub := &fakeHub{files: map[string][]byte{"flint-Q4_K_M.gguf": content, "flint-Q8_0.gguf": []byte("other")}}
	server := httptest.NewServer(hub.handler(t))
	defer server.Close()
	client := Hub{BaseURL: server.URL, Client: server.Client()}
	store := NewStore(t.TempDir())
	ctx := context.Background()

	hub.failAfter.Store(1000)
	if _, err := store.Pull(ctx, client, testModel(), "q4_k_m", "", nil); err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("interrupted pull: %v", err)
	}
	partial, err := os.Stat(store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf") + partialSuffix)
	if err != nil || partial.Size() != 1000 {
		t.Fatalf("partial file: %v %v", partial, err)
	}
	var last int64
	installed, err := store.Pull(ctx, client, testModel(), "q4_k_m", "", func(_ string, done, total int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if hub.ranges.Load() != 1 || last != int64(len(content)) || installed.Quant != "Q4_K_M" || installed.Size() != int64(len(content)) {
		t.Fatalf("resume: ranges %d, last %d, installed %+v", hub.ranges.Load(), last, installed)
	}
	data, err := os.ReadFile(store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf"))
	if err != nil || string(data) != string(content) {
		t.Fatal("downloaded file differs")
	}
	if _, err := store.Pull(ctx, client, testModel(), "Q4_K_M", "", nil); err != nil || hub.ranges.Load() != 1 {
		t.Fatalf("second pull downloaded again: %v", err)
	}
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("manifest = %+v, %v", list, err)
	}

	hub.digests = map[string]string{"flint-Q8_0.gguf": strings.Repeat("0", 64)}
	if _, err := store.Pull(ctx, client, testModel(), "Q8_0", "test/flint-GGUF", nil); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("corrupt pull: %v", err)
	}
	if _, err := os.Stat(store.Path("test/flint-GGUF", "flint-Q8_0.gguf") + partialSuffix); !os.IsNotExist(err) {
		t.Fatal("corrupt partial file kept")
	}
	if _, err := store.Pull(ctx, client, testModel(), "Q6_K", "", nil); err == nil || !strings.Contains(err.Error(), "no download") {
		t.Fatalf("missing quant: %v", err)
	}

	used, free, err := store.Usage()
	if err != nil || used < int64(len(content)) || free <= 0 {
		t.Fatalf("usage = %d, %d, %v", used, free, err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "stale.gguf.part"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if freed, err := store.Clean(); err != nil || freed != 5 {
		t.Fatalf("clean = %d, %v", freed, err)
	}
	if _, err := store.Remove("recursive-flint", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf")); !os.IsNotExist(err) {
		t.Fatal("removed model's file remains")
	}
	if _, err := store.Find("recursive-flint", ""); err == nil {
		t.Fatal("removed model still listed")
	}
}

func TestPullRestartsWithoutRangeSupport(t *testing.T) {
	content := []byte(strings.Repeat("x", 4096))
	hub := &fakeHub{files: map[string][]byte{"flint-Q4_K_M.gguf": content}, noRange: true}
	server := httptest.NewServer(hub.handler(t))
	defer server.Close()
	store := NewStore(t.TempDir())
	partial := store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf") + partialSuffix
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pull(context.Background(), Hub{BaseURL: server.URL, Client: server.Client()}, testModel(), "Q4_K_M", "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSelectQuant(t *testing.T) {
	files := []File{{Path: "flint-Q4_K_M.gguf"}, {Path: "flint-Q4_K_M_L.gguf"}, {Path: "Q8_0/flint-Q8_0-00002-of-00002.gguf"}, {Path: "Q8_0/flint-Q8_0-00001-of-00002.gguf"}, {Path: "mmproj-Q8_0.gguf"}, {Path: "flint-IQ3_XXS.gguf"}, {Path: "a-Q6_K.gguf"}, {Path: "b-Q6_K.gguf"}}
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
	for _, quant := range []string{"", "Q2_K", "Q6_K"} {
		if _, err := SelectQuant(files, quant); err == nil {
			t.Errorf("%q selected", quant)
		}
	}
}

func TestListFilesRejectsBadRepository(t *testing.T) {
	for _, repository := range []string{"", "a", "../x/y", "a/b/c", "a/.."} {
		if _, err := (Hub{}).ListFiles(context.Background(), repository); err == nil {
			t.Errorf("%q accepted", repository)
		}
	}
}

func TestLlamaVersion(t *testing.T) {
	version, err := ParseLlamaVersion("ggml_metal_init: ...\nversion: 11803 (a1b2c3d)\nbuilt with clang for arm64-apple-darwin")
	if err != nil || version.Build != 11803 || version.Commit != "a1b2c3d" {
		t.Fatalf("version = %+v, %v", version, err)
	}
	if _, err := ParseLlamaVersion("llama-server 1.0"); err == nil {
		t.Fatal("parsed nonsense")
	}
	flint, _ := catalog.Lookup("recursive-flint")
	if err := CheckRuntime(flint, LlamaVersion{Build: 11669}); err == nil || !strings.Contains(err.Error(), "b11801 or newer") {
		t.Fatalf("old build: %v", err)
	}
	if err := CheckRuntime(flint, LlamaVersion{Build: 11801}); err != nil {
		t.Fatal(err)
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
	written map[string]launchd.Agent
	started []string
	removed []string
}

func (manager *fakeManager) Write(agent launchd.Agent) error {
	manager.written[agent.Label] = agent
	return nil
}
func (manager *fakeManager) Start(label string) error {
	manager.started = append(manager.started, label)
	return nil
}
func (manager *fakeManager) Stop(string) {}
func (manager *fakeManager) Remove(label string) error {
	manager.removed = append(manager.removed, label)
	return nil
}
func (manager *fakeManager) State(string) string { return "running" }

func TestServices(t *testing.T) {
	store := NewStore(t.TempDir())
	model := testModel()
	installed := Installed{ID: model.ID, Quant: "Q4_K_M", Repository: "test/flint-GGUF", Files: []File{{Path: "flint-Q4_K_M.gguf"}}}
	agent, service, err := store.ServiceAgent(model, installed, ServeOptions{Binary: "/opt/llama/llama-server", LabelPrefix: "dev.overload", Port: 8080, Parallel: 2, LogDir: "/tmp/logs"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(agent.Args, " ")
	for _, want := range []string{"/opt/llama/llama-server -m " + store.Path("test/flint-GGUF", "flint-Q4_K_M.gguf"), "--host 127.0.0.1 --port 8080 --alias flint", "-c 524288", "--reasoning-format deepseek", "-np 2"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	if service.Label != "dev.overload.model.recursive-flint" || service.BaseURL() != "http://127.0.0.1:8080/v1" || service.ModelName != "flint" {
		t.Fatalf("service = %+v", service)
	}
	for _, options := range []ServeOptions{{Port: 80, Parallel: 1}, {Port: 8080, Parallel: 0}} {
		if _, _, err := store.ServiceAgent(model, installed, options); err == nil {
			t.Errorf("%+v accepted", options)
		}
	}
	manager := &fakeManager{written: map[string]launchd.Agent{}}
	if err := store.Serve(manager, agent, service); err != nil {
		t.Fatal(err)
	}
	other := Service{ID: "other", Label: "dev.overload.model.other", Port: 8080}
	if err := store.Serve(manager, launchd.Agent{Label: other.Label}, other); err == nil {
		t.Fatal("two services on one port")
	}
	services, err := store.Services()
	if err != nil || len(services) != 1 || len(manager.started) != 1 {
		t.Fatalf("services = %+v, %v", services, err)
	}
	if err := store.StopService(manager, "recursive-flint"); err != nil || len(manager.removed) != 1 {
		t.Fatalf("stop: %v", err)
	}
	if err := store.StopService(manager, "recursive-flint"); err == nil {
		t.Fatal("stopped a missing service")
	}
}

func TestHealth(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusServiceUnavailable)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(parsed.Port())
	service := Service{ID: "flint", Port: port}
	ctx := context.Background()
	if health := CheckHealth(ctx, server.Client(), service); health != HealthLoading {
		t.Fatalf("health = %s", health)
	}
	status.Store(http.StatusOK)
	if err := WaitReady(ctx, server.Client(), service, 0); err != nil {
		t.Fatal(err)
	}
	if health := CheckHealth(ctx, server.Client(), Service{Port: 1}); health != HealthUnreachable {
		t.Fatalf("health = %s", health)
	}
}
