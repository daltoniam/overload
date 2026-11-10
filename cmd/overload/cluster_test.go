package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

type fakeCluster struct{ placements []overload.Placement }

func (store *fakeCluster) SavePlacement(_ context.Context, placement overload.Placement) error {
	if err := placement.Validate(); err != nil {
		return err
	}
	store.placements = append(store.placements, placement)
	return nil
}
func (store *fakeCluster) ListPlacements(context.Context) ([]overload.Placement, error) {
	return store.placements, nil
}
func (store *fakeCluster) DeletePlacement(_ context.Context, name string) error {
	for index, placement := range store.placements {
		if placement.Name == name {
			store.placements = append(store.placements[:index], store.placements[index+1:]...)
			return nil
		}
	}
	return pgx.ErrNoRows
}

func TestClusterCommands(t *testing.T) {
	store := &fakeCluster{}
	var out bytes.Buffer
	rdma := filepath.Join(t.TempDir(), "rdma.json")
	if err := os.WriteFile(rdma, []byte(`[["","rdma_en5"],["rdma_en5",""]]`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := runCluster(ctx, store, []string{"place", "beam", "--model", "mlx-community/Beam-4bit", "--nodes", "a,b", "--rank0-ip", "10.0.0.1", "--rdma", rdma}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCluster(ctx, store, []string{"config", "beam"}, &out); err != nil || !strings.Contains(out.String(), "mlx.launch --backend jaccl --hostfile hostfile.json") || !strings.Contains(out.String(), `"ssh": "a"`) {
		t.Fatalf("config: %v %s", err, out.String())
	}
	for _, args := range [][]string{{}, {"config", "missing"}, {"place", "x", "--rdma", "/nope"}, {"place", "x", "--model", "mlx-community/Beam-4bit", "--nodes", "a,b", "--rank0-ip", "10.0.0.1", "--rdma", rdma, "--mode", "ring"}} {
		if err := runCluster(ctx, store, args, &out); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if err := runCluster(ctx, store, []string{"delete", "missing"}, &out); err == nil || err.Error() != "no placement named missing" {
		t.Fatalf("delete a missing placement: %v", err)
	}
	if err := runCluster(ctx, store, []string{"delete", "beam"}, &out); err != nil || len(store.placements) != 0 {
		t.Fatalf("delete: %v", err)
	}
}

// fakeProbe answers the commands a report runs, so tests never run sysctl,
// sw_vers or python3.
type fakeProbe struct {
	outputs map[string]string
	paths   map[string]string
	ran     []string
}

func (fake *fakeProbe) probe(goos string) nodeProbe {
	return nodeProbe{
		goos: goos,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			command := strings.Join(append([]string{name}, args...), " ")
			fake.ran = append(fake.ran, command)
			if out, ok := fake.outputs[command]; ok {
				return out, nil
			}
			return "", errors.New("not found")
		},
		lookPath: func(name string) (string, error) {
			if path, ok := fake.paths[name]; ok {
				return path, nil
			}
			return "", errors.New("not found")
		},
	}
}

const mlxCheck = " -c import mlx.core as mx; print(mx.__version__)"

func TestCollectReport(t *testing.T) {
	t.Setenv("OVERLOAD_MODELS_DIR", t.TempDir())
	t.Setenv("OVERLOAD_LLAMA_SERVER", "")
	mac := map[string]string{"sw_vers -productVersion": "26.2", "sysctl -n machdep.cpu.brand_string": "Apple M5 Ultra", "sysctl -n hw.memsize": "68719476736"}
	with := func(base map[string]string, extra map[string]string) map[string]string {
		merged := map[string]string{}
		for key, value := range base {
			merged[key] = value
		}
		for key, value := range extra {
			merged[key] = value
		}
		return merged
	}
	for _, test := range []struct {
		name      string
		goos      string
		python    string
		outputs   map[string]string
		paths     map[string]string
		mlx       string
		notRun    string
		osVersion string
	}{
		{name: "mac with the python3 stub and no developer tools", goos: "darwin", outputs: with(mac, map[string]string{"/usr/bin/python3" + mlxCheck: "0.30.1"}), paths: map[string]string{"python3": "/usr/bin/python3"}, notRun: "/usr/bin/python3" + mlxCheck, osVersion: "26.2"},
		{name: "mac with developer tools", goos: "darwin", outputs: with(mac, map[string]string{"xcode-select -p": "/Library/Developer/CommandLineTools", "/usr/bin/python3" + mlxCheck: "0.30.1"}), paths: map[string]string{"python3": "/usr/bin/python3"}, mlx: "0.30.1", osVersion: "26.2"},
		{name: "mac with Homebrew python", goos: "darwin", outputs: with(mac, map[string]string{"/opt/homebrew/bin/python3" + mlxCheck: "0.30.2"}), paths: map[string]string{"python3": "/opt/homebrew/bin/python3"}, mlx: "0.30.2", osVersion: "26.2"},
		{name: "configured virtual environment", goos: "darwin", python: "/opt/mlx/bin/python", outputs: with(mac, map[string]string{"/opt/mlx/bin/python" + mlxCheck: "0.31.0"}), mlx: "0.31.0", osVersion: "26.2"},
		{name: "linux", goos: "linux", outputs: map[string]string{"uname -r": "6.8.0"}, osVersion: "6.8.0"},
	} {
		fake := &fakeProbe{outputs: test.outputs, paths: test.paths}
		report := collectReport(context.Background(), fake.probe(test.goos), "studio-1", test.python)
		if report.Name != "studio-1" || report.OS != test.goos || report.OSVersion != test.osVersion || report.MLXVersion != test.mlx || report.OverloadVersion == "" || report.Validate() != nil {
			t.Errorf("%s: report = %+v", test.name, report)
		}
		if test.goos == "darwin" && report.MemoryBytes != 64<<30 {
			t.Errorf("%s: memory = %d", test.name, report.MemoryBytes)
		}
		if test.notRun != "" && slices.Contains(fake.ran, test.notRun) {
			t.Errorf("%s: ran %s", test.name, test.notRun)
		}
	}
}

func TestCollectReportFitsTheServerLimits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OVERLOAD_MODELS_DIR", dir)
	t.Setenv("OVERLOAD_LLAMA_SERVER", "")
	fake := &fakeProbe{outputs: map[string]string{"uname -r": strings.Repeat("6.8.0-", 100)}}
	report := collectReport(context.Background(), fake.probe("linux"), "studio-1", "")
	if err := report.Validate(); err != nil || len(report.OSVersion) != overload.MaxNodeTextLength {
		t.Fatalf("report = %+v, %v", report, err)
	}
}

func TestNodeEndpoint(t *testing.T) {
	for _, test := range []struct {
		server, want string
	}{
		{"https://overload.example.com/", "https://overload.example.com/api/nodes"},
		{"https://example.com/overload", "https://example.com/overload/api/nodes"},
		{"http://127.0.0.1:8082", "http://127.0.0.1:8082/api/nodes"},
		{"http://localhost:8082/", "http://localhost:8082/api/nodes"},
		{"http://[::1]:8082", "http://[::1]:8082/api/nodes"},
		{"ftp://x", ""},
		{"https://x/?a=b", ""},
		{"https://x/#top", ""},
		{"https://user:secret@x/", ""},
		{"http://overload.example.com", ""},
		{"overload.example.com", ""},
	} {
		got, err := nodeEndpoint(test.server)
		if got != test.want || (err == nil) != (test.want != "") {
			t.Errorf("%s: %q, %v", test.server, got, err)
		}
	}
}

func TestNodeCommandNeedsAValidName(t *testing.T) {
	original := hostname
	t.Cleanup(func() { hostname = original })
	hostname = func() (string, error) { return strings.Repeat("mac", 30) + ".local", nil }
	if err := manageNode([]string{"report", "--server", "https://x"}); err == nil || !strings.Contains(err.Error(), "pass --name") {
		t.Fatalf("an invalid host name was accepted: %v", err)
	}
	if err := manageNode([]string{"report", "--server", "ftp://x", "--name", "studio-1"}); err == nil || strings.Contains(err.Error(), "pass --name") {
		t.Fatalf("--name did not replace the host name: %v", err)
	}
}

func TestNodeCommandFlags(t *testing.T) {
	for _, args := range [][]string{
		{"report", "--server", "https://x", "--every", "1ms"},
		{"report", "--server", "https://x", "--name", "bad name"},
		{"install", "--server", "https://x", "--python", "python3"},
	} {
		if err := manageNode(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	agent := nodeAgent("dev.overload", "/usr/local/bin/overload", "https://x", "studio-1", "/opt/mlx/bin/python", t.TempDir(), 5*time.Minute)
	if agent.Label != "dev.overload.node" || agent.Env["PATH"] != servicePath || !strings.Contains(strings.Join(agent.Args, " "), "--every 5m0s --name studio-1 --python /opt/mlx/bin/python") {
		t.Fatalf("agent = %+v", agent)
	}
	if !slices.Contains((&installation{label: "dev.overload"}).serviceLabels(), "dev.overload.node") {
		t.Fatal("uninstall leaves the node reporter running")
	}
}

func TestSendReport(t *testing.T) {
	report := overload.NodeReport{Name: "studio-1", OS: "darwin"}
	var got overload.NodeReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ovl_node" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := sendReport(context.Background(), server.Client(), server.URL, "ovl_node", report); err != nil || got.Name != "studio-1" {
		t.Fatalf("send: %v %+v", err, got)
	}
	if err := sendReport(context.Background(), server.Client(), server.URL, "ovl_bad", report); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("send with a bad key: %v", err)
	}
}
