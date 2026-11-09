package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParsePRList(t *testing.T) {
	numbers, err := parsePRList("101, #102,110-112,101")
	if err != nil || !slices.Equal(numbers, []int{101, 102, 110, 111, 112}) {
		t.Fatalf("numbers = %v %v", numbers, err)
	}
	for _, bad := range []string{"", "abc", "5-3", "0", "1-5000"} {
		if _, err := parsePRList(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseEvalOptions(t *testing.T) {
	options, err := parseEvalOptions([]string{"prs", "--repo", "acme/app", "--prs", "1,2", "--workflows", "sol, sonnet,sol", "--out", "results.json"})
	if err != nil || !slices.Equal(options.workflows, []string{"sol", "sonnet"}) || options.csv != "results.csv" || options.concurrency != 2 {
		t.Fatalf("options = %+v %v", options, err)
	}
	if _, err := parseEvalOptions([]string{"prs", "--repo", "acme/app", "--prs", "1"}); err == nil {
		t.Fatal("accepted options without workflows and output")
	}
}

func TestReadLabels(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "labels.json")
	if err := os.WriteFile(good, []byte(`[{"pr":101,"path":"a.py","line":12,"window":3,"keywords":["null"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	labels, err := readLabels(good)
	if err != nil || len(labels) != 1 || labels[0].Window != 3 {
		t.Fatalf("labels = %+v %v", labels, err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`[{"pr":101,"path":"a.py","line":0}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLabels(bad); err == nil {
		t.Fatal("accepted a label without a line")
	}
}
