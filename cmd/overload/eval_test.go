package main

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/daltoniam/overload/eval"
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
	largest := strconv.Itoa(math.MaxInt)
	if numbers, err := parsePRList(largest); err != nil || !slices.Equal(numbers, []int{math.MaxInt}) {
		t.Fatalf("largest number = %v %v", numbers, err)
	}
	if numbers, err := parsePRList(strconv.Itoa(math.MaxInt-1) + "-" + largest); err != nil || len(numbers) != 2 {
		t.Fatalf("range ending at the largest number = %v %v", numbers, err)
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
	for _, flags := range [][]string{
		{"--out", "results.csv", "--csv", "results.csv"},
		{"--out", "results.json", "--csv", "results.json"},
		{"--out", "out/results.json", "--csv", "out/../out/results.json"},
	} {
		args := append([]string{"prs", "--repo", "acme/app", "--prs", "1", "--workflows", "sol"}, flags...)
		if _, err := parseEvalOptions(args); err == nil {
			t.Errorf("accepted the same file for JSON and CSV: %v", flags)
		}
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

func TestWriteEvalReportRefusesAliasedOutputs(t *testing.T) {
	dir := t.TempDir()
	jsonPath, csvPath := filepath.Join(dir, "results.json"), filepath.Join(dir, "results.csv")
	// results.csv is a dangling symlink to results.json until the JSON is
	// written, so the paths look different when options are parsed.
	if err := os.Symlink(jsonPath, csvPath); err != nil {
		t.Fatal(err)
	}
	if _, err := parseEvalOptions([]string{"prs", "--repo", "acme/app", "--prs", "1", "--workflows", "sol", "--out", jsonPath}); err != nil {
		t.Fatalf("options: %v", err)
	}
	report := eval.Report{Repository: "acme/app"}
	if err := writeEvalReport(report, jsonPath, csvPath); err == nil {
		t.Fatal("wrote the CSV over the JSON report")
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil || !strings.Contains(string(data), `"acme/app"`) {
		t.Fatalf("JSON report lost: %q %v", data, err)
	}
	other := filepath.Join(dir, "summary.csv")
	if err := writeEvalReport(report, jsonPath, other); err != nil {
		t.Fatal(err)
	}
}
