package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/postgres/pgtest"
)

func TestNodesAndPlacements(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	key, _, err := store.CreateAPIKey(ctx, 0, "nodes", []string{overload.ScopeNodes}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := store.CreateAPIKey(ctx, 0, "other nodes", []string{overload.ScopeNodes}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"studio-1", "studio-2"} {
		if err := store.ReportNode(ctx, key.ID, overload.NodeReport{Name: name, OS: "darwin", OSVersion: "26.2"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReportNode(ctx, key.ID, overload.NodeReport{Name: "studio-1", OS: "darwin", OSVersion: "26.3"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNode(ctx, other.ID, overload.NodeReport{Name: "studio-1", OS: "darwin", OSVersion: "1.0"}); !errors.Is(err, overload.ErrNodeClaimed) {
		t.Fatalf("another key overwrote a node: %v", err)
	}
	if err := store.ReportNode(ctx, key.ID, overload.NodeReport{Name: "bad name"}); !errors.Is(err, overload.ErrInvalidNodeReport) {
		t.Fatalf("stored an invalid report: %v", err)
	}
	nodes, err := store.ListNodes(ctx)
	if err != nil || len(nodes) != 2 || nodes[0].Report.OSVersion != "26.3" {
		t.Fatalf("nodes = %+v, %v", nodes, err)
	}
	placement := overload.Placement{Name: "beam", Model: "mlx-community/Beam-4bit", Nodes: []string{"studio-1", "studio-3"}, Rank0IP: "10.0.0.1", RDMA: [][]string{{"", "rdma_en5"}, {"rdma_en5", ""}}}
	if err := store.SavePlacement(ctx, placement); err == nil {
		t.Fatal("placed a model on a node that never reported")
	}
	placement.Nodes[1] = "studio-2"
	if err := store.SavePlacement(ctx, placement); err != nil {
		t.Fatal(err)
	}
	placements, err := store.ListPlacements(ctx)
	if err != nil || len(placements) != 1 || placements[0].Nodes[1] != "studio-2" {
		t.Fatalf("placements = %+v, %v", placements, err)
	}
	if err := store.DeleteNode(ctx, "studio-2"); !errors.Is(err, overload.ErrNodeInPlacement) {
		t.Fatalf("forgot a node a placement uses: %v", err)
	}
	if err := store.DeletePlacement(ctx, "beam"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteNode(ctx, "studio-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteNode(ctx, "studio-1"); err == nil {
		t.Fatal("deleted a missing node")
	}
	if err := store.ReportNode(ctx, other.ID, overload.NodeReport{Name: "studio-1", OS: "darwin"}); err != nil {
		t.Fatalf("forgetting a node did not release its name: %v", err)
	}
}

func TestNodeKeyRotation(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	createKey := func(name string) overload.APIKey {
		key, _, err := store.CreateAPIKey(ctx, 0, name, []string{overload.ScopeNodes}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	old, replacement, third := createKey("old"), createKey("new"), createKey("third")
	for _, name := range []string{"studio-1", "studio-2"} {
		if err := store.ReportNode(ctx, old.ID, overload.NodeReport{Name: name, OS: "darwin"}); err != nil {
			t.Fatal(err)
		}
	}
	placement := overload.Placement{Name: "beam", Model: "mlx-community/Beam-4bit", Nodes: []string{"studio-1", "studio-2"}, Rank0IP: "10.0.0.1", RDMA: [][]string{{"", "rdma_en5"}, {"rdma_en5", ""}}}
	if err := store.SavePlacement(ctx, placement); err != nil {
		t.Fatal(err)
	}
	if nodes, err := store.ListNodes(ctx); err != nil || nodes[0].KeyPrefix != old.Prefix {
		t.Fatalf("nodes = %+v, %v", nodes, err)
	}
	if err := store.ReportNode(ctx, replacement.ID, overload.NodeReport{Name: "studio-1", OS: "darwin"}); !errors.Is(err, overload.ErrNodeClaimed) {
		t.Fatalf("a new key took a node from a valid key: %v", err)
	}
	if err := store.RevokeAPIKey(ctx, old.ID, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNode(ctx, replacement.ID, overload.NodeReport{Name: "studio-1", OS: "darwin", OSVersion: "26.3"}); err != nil {
		t.Fatalf("the new key cannot take over from a revoked key: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE id = $1`, replacement.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNode(ctx, third.ID, overload.NodeReport{Name: "studio-1", OS: "darwin"}); err != nil {
		t.Fatalf("a key cannot take over from an expired key: %v", err)
	}
	fourth := createKey("fourth")
	if err := store.ReportNode(ctx, fourth.ID, overload.NodeReport{Name: "studio-1", OS: "darwin"}); !errors.Is(err, overload.ErrNodeClaimed) {
		t.Fatalf("took a node from a valid key: %v", err)
	}
	if err := store.ReleaseNode(ctx, "studio-1"); err != nil {
		t.Fatal(err)
	}
	if nodes, err := store.ListNodes(ctx); err != nil || nodes[0].KeyPrefix != "" {
		t.Fatalf("released node still held: %+v, %v", nodes, err)
	}
	if err := store.ReportNode(ctx, third.ID, overload.NodeReport{Name: "studio-1", OS: "darwin"}); !errors.Is(err, overload.ErrNodeReleased) {
		t.Fatalf("the released key took its node back: %v", err)
	}
	if err := store.ReportNode(ctx, fourth.ID, overload.NodeReport{Name: "studio-1", OS: "darwin"}); err != nil {
		t.Fatalf("a released node was not taken over: %v", err)
	}
	if nodes, err := store.ListNodes(ctx); err != nil || nodes[0].KeyPrefix != fourth.Prefix {
		t.Fatalf("nodes = %+v, %v", nodes, err)
	}
	if err := store.ReleaseNode(ctx, "studio-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNode(ctx, third.ID, overload.NodeReport{Name: "studio-1", OS: "darwin"}); err != nil {
		t.Fatalf("a key released earlier could not claim a name released from another key: %v", err)
	}
	if placements, err := store.ListPlacements(ctx); err != nil || len(placements) != 1 {
		t.Fatalf("rotation lost the placement: %+v, %v", placements, err)
	}
	if err := store.ReleaseNode(ctx, "missing"); err == nil {
		t.Fatal("released a missing node")
	}
}

func TestNodeLimits(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	key, _, err := store.CreateAPIKey(ctx, 0, "nodes", []string{overload.ScopeNodes}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := store.CreateAPIKey(ctx, 0, "other nodes", []string{overload.ScopeNodes}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO nodes (name, report, api_key_id) SELECT 'node-' || n, '{}', $1 FROM generate_series(1, $2) AS n`, key.ID, postgres.MaxNodesPerKey); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNode(ctx, key.ID, overload.NodeReport{Name: "one-too-many"}); !errors.Is(err, overload.ErrTooManyNodes) {
		t.Fatalf("a key claimed more than its share: %v", err)
	}
	if err := store.ReportNode(ctx, key.ID, overload.NodeReport{Name: "node-1"}); err != nil {
		t.Fatalf("a full key cannot update its own node: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO nodes (name, report) SELECT 'unclaimed-' || n, '{}' FROM generate_series(1, $1) AS n`, postgres.MaxNodes-postgres.MaxNodesPerKey); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNode(ctx, other.ID, overload.NodeReport{Name: "new"}); !errors.Is(err, overload.ErrTooManyNodes) {
		t.Fatalf("the inventory grew past its limit: %v", err)
	}
	if err := store.ReportNode(ctx, other.ID, overload.NodeReport{Name: "unclaimed-1"}); err != nil {
		t.Fatalf("a full inventory refused a known node: %v", err)
	}
	if err := store.ReportNode(ctx, key.ID, overload.NodeReport{Name: "unclaimed-2"}); !errors.Is(err, overload.ErrTooManyNodes) {
		t.Fatalf("a full key took over an unclaimed node: %v", err)
	}
	if err := store.ReleaseNode(ctx, "unclaimed-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNode(ctx, key.ID, overload.NodeReport{Name: "unclaimed-1"}); !errors.Is(err, overload.ErrTooManyNodes) {
		t.Fatalf("a full key took over a released node: %v", err)
	}
}
