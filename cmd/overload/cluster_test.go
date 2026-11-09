package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
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
func (store *fakeCluster) DeletePlacement(context.Context, string) error { return nil }

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
	for _, args := range [][]string{{}, {"config", "missing"}, {"place", "x", "--rdma", "/nope"}} {
		if err := runCluster(ctx, store, args, &out); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestNodeReporting(t *testing.T) {
	if _, err := nodeEndpoint("ftp://x"); err == nil {
		t.Fatal("accepted a non-http server")
	}
	target, err := nodeEndpoint("https://overload.example.com/")
	if err != nil || target != "https://overload.example.com/api/nodes" {
		t.Fatalf("endpoint = %s, %v", target, err)
	}
	t.Setenv("OVERLOAD_MODELS_DIR", t.TempDir())
	t.Setenv("OVERLOAD_LLAMA_SERVER", "/nonexistent/llama-server")
	report := collectReport(context.Background(), "studio-1")
	if report.Name != "studio-1" || report.OS == "" || report.OverloadVersion == "" || report.Validate() != nil {
		t.Fatalf("report = %+v", report)
	}
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
