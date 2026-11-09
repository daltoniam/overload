package postgres_test

import (
	"context"
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres/pgtest"
)

func TestNodesAndPlacements(t *testing.T) {
	store := pgtest.Isolated(t)
	ctx := context.Background()
	for _, name := range []string{"studio-1", "studio-2"} {
		if err := store.ReportNode(ctx, overload.NodeReport{Name: name, OS: "darwin", OSVersion: "26.2"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReportNode(ctx, overload.NodeReport{Name: "studio-1", OS: "darwin", OSVersion: "26.3"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReportNode(ctx, overload.NodeReport{Name: "bad name"}); err == nil {
		t.Fatal("stored an invalid report")
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
	if err := store.DeletePlacement(ctx, "beam"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteNode(ctx, "studio-2"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteNode(ctx, "studio-2"); err == nil {
		t.Fatal("deleted a missing node")
	}
}
