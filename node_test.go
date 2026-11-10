package overload

import (
	"errors"
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
		"bad name":       {Name: "../x"},
		"multi-line":     {Name: "a", Chip: "a\nb"},
		"negative":       {Name: "a", MemoryBytes: -1},
		"long":           {Name: "a", OSVersion: strings.Repeat("x", MaxNodeTextLength+1)},
		"many downloads": {Name: "a", Downloads: make([]string, MaxNodeDownloads+1)},
		"long model":     {Name: "a", Services: []NodeService{{Model: strings.Repeat("m", MaxNodeServiceModel+1)}}},
	} {
		if err := report.Validate(); !errors.Is(err, ErrInvalidNodeReport) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNodeReportBounded(t *testing.T) {
	report := NodeReport{Name: "studio-1", Chip: "Apple\nM5", OSVersion: "x" + strings.Repeat("é", MaxNodeTextLength)}
	for range MaxNodeDownloads + 5 {
		report.Downloads = append(report.Downloads, strings.Repeat("d", MaxNodeDownloadLength+10))
	}
	for range MaxNodeServices + 5 {
		report.Services = append(report.Services, NodeService{Model: strings.Repeat("m", MaxNodeServiceModel+10), Endpoint: "http://127.0.0.1:8081/v1", Health: strings.Repeat("h", 50)})
	}
	bounded := report.Bounded()
	if err := bounded.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(bounded.Downloads) != MaxNodeDownloads || len(bounded.Services) != MaxNodeServices || bounded.Chip != "Apple M5" || len(report.Downloads) != MaxNodeDownloads+5 {
		t.Fatalf("bounded = %d downloads, %d services, chip %q", len(bounded.Downloads), len(bounded.Services), bounded.Chip)
	}
}

func TestDrift(t *testing.T) {
	now := time.Now()
	node := func(name, system, osVersion string, build int, seen time.Duration) Node {
		return Node{Report: NodeReport{Name: name, OS: system, OSVersion: osVersion, LlamaBuild: build, OverloadVersion: "1.0"}, LastSeen: now.Add(-seen)}
	}
	for _, test := range []struct {
		name  string
		nodes []Node
		want  []VersionDrift
	}{
		{
			name:  "one node behind",
			nodes: []Node{node("a", "darwin", "26.2", 12058, 0), node("b", "darwin", "26.2", 12058, 0), node("c", "darwin", "26.1", 12144, 0), node("d", "darwin", "25.0", 1, time.Hour)},
			want:  []VersionDrift{{Field: "os_version", OS: "darwin", Nodes: []string{"c"}}, {Field: "llama_build", Nodes: []string{"c"}}},
		},
		{
			name:  "linux is not compared with macOS",
			nodes: []Node{node("a", "darwin", "26.2", 0, 0), node("b", "darwin", "26.2", 0, 0), node("gpu", "linux", "6.8.0", 0, 0)},
		},
		{
			name:  "an even split names no culprit",
			nodes: []Node{node("a", "darwin", "26.9", 0, 0), node("b", "darwin", "26.10", 0, 0)},
			want:  []VersionDrift{{Field: "os_version", OS: "darwin", Nodes: []string{"a", "b"}, Even: true}},
		},
	} {
		got := Drift(test.nodes, now)
		if len(got) != len(test.want) {
			t.Errorf("%s: drift = %+v", test.name, got)
			continue
		}
		for index := range got {
			if got[index].Field != test.want[index].Field || got[index].OS != test.want[index].OS || got[index].Even != test.want[index].Even || strings.Join(got[index].Nodes, ",") != strings.Join(test.want[index].Nodes, ",") {
				t.Errorf("%s: drift = %+v", test.name, got)
			}
		}
	}
	if !node("x", "", "", 0, time.Minute).Online(now) || node("x", "", "", 0, time.Hour).Online(now) {
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
	if command := placement.Command("hosts.json"); command != "mlx.launch --backend jaccl --hostfile hosts.json -- /opt/mlx/bin/python -m mlx_lm.server --model mlx-community/Beam-4bit --host 192.168.1.10 --port 8080" {
		t.Fatalf("command = %s", command)
	}
	served := placement
	served.Host, served.Port, served.Mode = "10.0.0.5", 9000, PlacementPipeline
	if command := served.Command("hosts.json"); !strings.HasSuffix(command, "--host 10.0.0.5 --port 9000 --pipeline") || served.BaseURL() != "http://10.0.0.5:9000/v1" {
		t.Fatalf("command = %s, base %s", command, served.BaseURL())
	}
	quoted := placement
	quoted.Python = "/Users/ann/My Envs/mlx/bin/python"
	if command := quoted.Command("/tmp/it's here/hosts.json"); !strings.Contains(command, `--hostfile '/tmp/it'\''s here/hosts.json' -- '/Users/ann/My Envs/mlx/bin/python' -m mlx_lm.server`) {
		t.Fatalf("command = %s", command)
	}
	for _, test := range []struct{ host, want string }{
		{"0.0.0.0", "http://192.168.1.10:8080/v1"},
		{"::", "http://192.168.1.10:8080/v1"},
		{"10.0.0.5", "http://10.0.0.5:8080/v1"},
		{"fd00::5", "http://[fd00::5]:8080/v1"},
	} {
		bound := placement
		bound.Host = test.host
		if got := bound.BaseURL(); got != test.want || !strings.Contains(bound.Command("hosts.json"), "--host "+shellQuote(test.host)+" ") {
			t.Errorf("%s: base %s, command %s", test.host, got, bound.Command("hosts.json"))
		}
	}
	for name, change := range map[string]func(*Placement){
		"one node":      func(p *Placement) { p.Nodes, p.RDMA = p.Nodes[:1], p.RDMA[:1] },
		"five nodes":    func(p *Placement) { p.Nodes = []string{"a", "b", "c", "d", "e"} },
		"bad model":     func(p *Placement) { p.Model = "beam; rm -rf /" },
		"bad ip":        func(p *Placement) { p.Rank0IP = "studio-1" },
		"bad host":      func(p *Placement) { p.Host = "0.0.0.0 --trust-remote-code" },
		"bad port":      func(p *Placement) { p.Port = 70000 },
		"bad mode":      func(p *Placement) { p.Mode = "ring" },
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
		if _, err := invalid.Hostfile(); err == nil {
			t.Errorf("%s: hostfile generated", name)
		}
	}
}
