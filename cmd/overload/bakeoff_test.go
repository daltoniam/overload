package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/postgres"
)

type fakeBakeoffs struct {
	saved  []eval.Suite
	queued []string
}

func (store *fakeBakeoffs) SaveSuite(_ context.Context, suite eval.Suite) error {
	if err := suite.Validate(); err != nil {
		return err
	}
	store.saved = append(store.saved, suite)
	return nil
}
func (store *fakeBakeoffs) ListSuites(context.Context) ([]eval.Suite, error) {
	return append([]eval.Suite{eval.PlantedSuite()}, store.saved...), nil
}
func (store *fakeBakeoffs) DeleteSuite(context.Context, string) error { return nil }
func (store *fakeBakeoffs) GetBakeoff(context.Context, int64) (postgres.Bakeoff, error) {
	return postgres.Bakeoff{ID: 4, Status: "completed", Models: []overload.ModelProfile{{Name: "flint"}}, Results: []postgres.BakeoffResult{{Model: "flint", Score: eval.Score{Expected: 5, Matched: 3}}}}, nil
}
func (store *fakeBakeoffs) queueBakeoff(_ context.Context, models, suites []string, runs int) (int64, error) {
	store.queued = append(append(store.queued, models...), suites...)
	return 4, nil
}

func TestBakeoffCommands(t *testing.T) {
	store := &fakeBakeoffs{}
	var out bytes.Buffer
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "case.json")
	data, _ := json.Marshal(eval.PlantedSuite().Cases[1])
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runBakeoffWith(ctx, store, []string{"suite", "save", "ours", "--description", "real bugs", path}, &out); err != nil || len(store.saved) != 1 || store.saved[0].Description != "real bugs" {
		t.Fatalf("save: %v %+v", err, store.saved)
	}
	out.Reset()
	if err := runBakeoffWith(ctx, store, []string{"suite", "list"}, &out); err != nil || !strings.Contains(out.String(), "ours") || !strings.Contains(out.String(), "planted") {
		t.Fatalf("list: %v %s", err, out.String())
	}
	if err := runBakeoffWith(ctx, store, []string{"run", "--models", "flint,gemma", "--suites", "planted,ours", "--runs", "2"}, &out); err != nil || strings.Join(store.queued, ",") != "flint,gemma,planted,ours" {
		t.Fatalf("run: %v %v", err, store.queued)
	}
	out.Reset()
	if err := runBakeoffWith(ctx, store, []string{"show", "4"}, &out); err != nil || !strings.Contains(out.String(), "3 of 5") {
		t.Fatalf("show: %v %s", err, out.String())
	}
	for _, args := range [][]string{{}, {"run"}, {"show", "x"}, {"suite", "save"}, {"bogus"}} {
		if err := runBakeoffWith(ctx, store, args, &out); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if err := bakeoffCase(ctx, []string{"--repo", t.TempDir()}, &out); err == nil {
		t.Error("case without a commit accepted")
	}
}
