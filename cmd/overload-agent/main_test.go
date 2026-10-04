package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
)

func TestHeaderEnvironmentDoesNotLeakIntoResult(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.json")
	outputPath := filepath.Join(dir, "result.json")
	input, err := json.Marshal(overload.ReviewSpec{Workflow: overload.ResolvedWorkflow{Agents: []overload.ResolvedAgent{{Model: overload.ModelProfile{Headers: map[string]string{"cf-aig-metadata": "private-example"}}}}}})
	if err != nil || strings.Contains(string(input), "private-example") {
		t.Fatalf("secret persisted in spec: %v", err)
	}
	if err := os.WriteFile(specPath, input, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVERLOAD_MODEL_HEADERS_JSON", `{"authorization":"Bearer private-example"}`)
	if err := run([]string{"review", "--spec", specPath, "--repo", dir, "--out", outputPath}); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil || strings.Contains(string(output), "private-example") {
		t.Fatalf("secret persisted in result: %v", err)
	}
	t.Setenv("OVERLOAD_MODEL_HEADERS_JSON", "invalid")
	if err := run([]string{"review", "--spec", specPath, "--repo", dir, "--out", outputPath}); err == nil || strings.Contains(err.Error(), "private-example") {
		t.Fatalf("malformed header environment accepted or leaked: %v", err)
	}
}

func TestReviewTimeout(t *testing.T) {
	for _, test := range []struct {
		name  string
		value time.Duration
		valid bool
	}{
		{"default", 15 * time.Minute, true},
		{"extended", 45 * time.Minute, true},
		{"zero", 0, false},
		{"long PR", 4*time.Hour + 20*time.Minute, true},
		{"excessive", 6 * time.Hour, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validReviewTimeout(test.value); got != test.valid {
				t.Fatalf("validReviewTimeout(%s)=%v, want %v", test.value, got, test.valid)
			}
		})
	}
}
