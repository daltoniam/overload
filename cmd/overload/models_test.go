package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/launchd"
	"github.com/daltoniam/overload/localmodels"
)

type recordingManager struct{ agents map[string]launchd.Agent }

func (manager *recordingManager) Write(agent launchd.Agent) error {
	manager.agents[agent.Label] = agent
	return nil
}
func (manager *recordingManager) Start(string) error { return nil }
func (manager *recordingManager) Stop(string)        {}
func (manager *recordingManager) Remove(label string) error {
	delete(manager.agents, label)
	return nil
}
func (manager *recordingManager) State(label string) string {
	if _, ok := manager.agents[label]; ok {
		return "running"
	}
	return "not loaded"
}

func TestModelsCommand(t *testing.T) {
	weights := []byte(strings.Repeat("lfm", 1000))
	sum := sha256.Sum256(weights)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/LiquidAI/LFM2.5-24B-A2B-GGUF/tree/main":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "LFM2.5-24B-A2B-Q8_0.gguf", "lfs": map[string]any{"oid": hex.EncodeToString(sum[:]), "size": len(weights)}}})
		case "/LiquidAI/LFM2.5-24B-A2B-GGUF/resolve/main/LFM2.5-24B-A2B-Q8_0.gguf":
			_, _ = w.Write(weights)
		case "/health":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	dir := t.TempDir()
	llama := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(llama, []byte("#!/bin/sh\necho 'version: 12058 (deadbee)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVERLOAD_MODELS_DIR", filepath.Join(dir, "models"))
	t.Setenv("OVERLOAD_LLAMA_SERVER", llama)
	t.Setenv("OVERLOAD_INSTALL_LABEL", "test.overload")
	var out bytes.Buffer
	manager := &recordingManager{agents: map[string]launchd.Agent{}}
	var saved overload.ReviewSettings
	command := modelsCommand{out: &out, hub: localmodels.Hub{BaseURL: server.URL, Client: server.Client()}, manager: manager, client: server.Client(), goos: "darwin", connect: func(_ context.Context, setting overload.ReviewSettings) error {
		saved = setting
		return setting.Validate()
	}}
	run := func(args ...string) error {
		out.Reset()
		return command.run(context.Background(), args)
	}
	if err := run("pull", "lfm2.5-24b-a2b"); err == nil || !strings.Contains(err.Error(), "--quant") {
		t.Fatalf("pull without quant: %v", err)
	}
	if err := run("pull", "nope", "--quant", "Q8_0"); err == nil {
		t.Fatal("pulled an unknown model")
	}
	if err := run("pull", "lfm2.5-24b-a2b", "--quant", "Q8_0"); err != nil || !strings.Contains(out.String(), "Downloaded lfm2.5-24b-a2b Q8_0") {
		t.Fatalf("pull: %v %s", err, out.String())
	}
	if err := run("list"); err != nil || !strings.Contains(out.String(), "LiquidAI/LFM2.5-24B-A2B-GGUF") || !strings.Contains(out.String(), "free on the volume") {
		t.Fatalf("list: %v %s", err, out.String())
	}
	if err := run("check", "lfm2.5-24b-a2b"); err != nil || !strings.Contains(out.String(), "b12058") {
		t.Fatalf("check: %v %s", err, out.String())
	}
	if err := run("check", "recursive-flint"); err != nil {
		t.Fatalf("check flint on b12058: %v", err)
	}
	if err := run("serve", "lfm2.5-24b-a2b", "--port", parsed.Port(), "--parallel", "2", "--connect", "lfm"); err != nil {
		t.Fatalf("serve: %v %s", err, out.String())
	}
	agent, ok := manager.agents["test.overload.model.lfm2.5-24b-a2b"]
	if !ok || !strings.Contains(strings.Join(agent.Args, " "), "-c 262144") || agent.Args[0] != llama {
		t.Fatalf("agent = %+v", agent)
	}
	port, _ := strconv.Atoi(parsed.Port())
	if saved.Name != "lfm" || saved.BaseURL != "http://127.0.0.1:"+strconv.Itoa(port)+"/v1" || saved.Catalog != "lfm2.5-24b-a2b" || saved.Concurrency != 2 || saved.ReasoningParam != "none" || saved.Model != "lfm2.5-24b-a2b" {
		t.Fatalf("saved connection = %+v", saved)
	}
	summary, err := localModelSummary(localmodels.NewStore(filepath.Join(dir, "models")), manager, "lfm2.5-24b-a2b")
	if err != nil || len(summary.Quants) != 1 || summary.Endpoint == "" || summary.Service != "running" {
		t.Fatalf("summary = %+v, %v", summary, err)
	}
	if err := run("status"); err != nil || !strings.Contains(out.String(), "running") || !strings.Contains(out.String(), "ready") {
		t.Fatalf("status: %v %s", err, out.String())
	}
	if err := run("rm", "lfm2.5-24b-a2b"); err == nil || !strings.Contains(err.Error(), "being served") {
		t.Fatalf("removed a served model: %v", err)
	}
	if err := run("stop", "lfm2.5-24b-a2b"); err != nil {
		t.Fatal(err)
	}
	if err := run("rm", "lfm2.5-24b-a2b"); err != nil || !strings.Contains(out.String(), "Removed lfm2.5-24b-a2b Q8_0") {
		t.Fatalf("rm: %v %s", err, out.String())
	}
	if err := run("clean"); err != nil {
		t.Fatal(err)
	}
	command.goos = "linux"
	if err := run("serve", "lfm2.5-24b-a2b"); err == nil || !strings.Contains(err.Error(), "macOS") {
		t.Fatalf("serve on linux: %v", err)
	}
	if err := run("bogus"); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}
