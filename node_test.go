package overload

import (
	"strings"
	"testing"
	"time"
)

func TestNodeReportValidate(t *testing.T) {
	good := NodeReport{Name: "studio-1", OS: "darwin", OSVersion: "26.2", Chip: "Apple M5 Ultra", MemoryBytes: 512e9}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, report := range map[string]NodeReport{
		"bad name":   {Name: "../x"},
		"multi-line": {Name: "a", Chip: "a\nb"},
		"negative":   {Name: "a", MemoryBytes: -1},
		"long":       {Name: "a", OSVersion: strings.Repeat("x", 201)},
	} {
		if err := report.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
}

func TestDrift(t *testing.T) {
	now := time.Now()
	node := func(name, osVersion string, build int, seen time.Duration) Node {
		return Node{Report: NodeReport{Name: name, OSVersion: osVersion, LlamaBuild: build, OverloadVersion: "1.0"}, LastSeen: now.Add(-seen)}
	}
	drift := Drift([]Node{node("a", "26.2", 12058, 0), node("b", "26.2", 12058, 0), node("c", "26.1", 12144, 0), node("d", "25.0", 1, time.Hour)}, now)
	if strings.Join(drift["os_version"], ",") != "c" || strings.Join(drift["llama_build"], ",") != "c" || len(drift["overload_version"]) != 0 {
		t.Fatalf("drift = %v", drift)
	}
	if !node("x", "", 0, time.Minute).Online(now) || node("x", "", 0, time.Hour).Online(now) {
		t.Fatal("Online is wrong")
	}
}

func TestPlacement(t *testing.T) {
	placement := Placement{Name: "beam", Model: "mlx-community/Beam-4bit", Nodes: []string{"studio-1", "studio-2"}, Rank0IP: "192.168.1.10", RDMA: [][]string{{"", "rdma_en5"}, {"rdma_en5", ""}}, Python: "/opt/mlx/bin/python"}
	if err := placement.Validate(); err != nil {
		t.Fatal(err)
	}
	hostfile, err := placement.Hostfile()
	if err != nil || !strings.Contains(string(hostfile), `"ips": [
      "192.168.1.10"
    ]`) || !strings.Contains(string(hostfile), `"rdma": [
      null,
      "rdma_en5"
    ]`) {
		t.Fatalf("hostfile = %s", hostfile)
	}
	if command := placement.Command("hosts.json"); command != "mlx.launch --backend jaccl --hostfile hosts.json -- /opt/mlx/bin/python -m mlx_lm.server --model mlx-community/Beam-4bit" {
		t.Fatalf("command = %s", command)
	}
	for name, change := range map[string]func(*Placement){
		"one node":      func(p *Placement) { p.Nodes, p.RDMA = p.Nodes[:1], p.RDMA[:1] },
		"bad model":     func(p *Placement) { p.Model = "beam; rm -rf /" },
		"bad ip":        func(p *Placement) { p.Rank0IP = "studio-1" },
		"short rdma":    func(p *Placement) { p.RDMA = p.RDMA[:1] },
		"self device":   func(p *Placement) { p.RDMA = [][]string{{"rdma_en5", "rdma_en5"}, {"rdma_en5", ""}} },
		"missing link":  func(p *Placement) { p.RDMA = [][]string{{"", ""}, {"rdma_en5", ""}} },
		"bad device":    func(p *Placement) { p.RDMA = [][]string{{"", "en5; x"}, {"rdma_en5", ""}} },
		"repeated node": func(p *Placement) { p.Nodes = []string{"a", "a"} },
		"relative py":   func(p *Placement) { p.Python = "python3" },
	} {
		invalid := placement
		invalid.Nodes = append([]string(nil), placement.Nodes...)
		change(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
}
