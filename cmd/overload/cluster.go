package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/daltoniam/overload"
)

const clusterUsage = `usage: overload cluster COMMAND
  place NAME --model REPO --nodes a,b --rank0-ip IP --rdma rdma.json [--python /path/to/python]
  config NAME          print the mlx.launch hostfile and command
  list | delete NAME`

// clusterStore is what the cluster commands need.
type clusterStore interface {
	SavePlacement(context.Context, overload.Placement) error
	ListPlacements(context.Context) ([]overload.Placement, error)
	DeletePlacement(context.Context, string) error
}

func manageCluster(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	return runCluster(ctx, store, args, os.Stdout)
}

func runCluster(ctx context.Context, store clusterStore, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(clusterUsage)
	}
	switch {
	case args[0] == "place" && len(args) >= 2:
		flags := flag.NewFlagSet("cluster place", flag.ContinueOnError)
		model := flags.String("model", "", "MLX model repository, such as mlx-community/Model-4bit")
		nodes := flags.String("nodes", "", "comma-separated node names in rank order")
		rank0 := flags.String("rank0-ip", "", "an IP address of the first node every node can reach")
		rdma := flags.String("rdma", "", "JSON file: rdma[i][j] is node i's RDMA device reaching node j, empty for itself")
		python := flags.String("python", "", "Python interpreter path, the same on every node")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		placement := overload.Placement{Name: args[1], Model: *model, Nodes: strings.Split(*nodes, ","), Rank0IP: *rank0, Python: *python}
		data, err := os.ReadFile(*rdma)
		if err != nil {
			return fmt.Errorf("--rdma: %w", err)
		}
		if err := json.Unmarshal(data, &placement.RDMA); err != nil {
			return fmt.Errorf("--rdma: %w", err)
		}
		if err := store.SavePlacement(ctx, placement); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Saved placement %s; print its launch configuration with overload cluster config %s\n", placement.Name, placement.Name)
		return err
	case args[0] == "config" && len(args) == 2, args[0] == "delete" && len(args) == 2:
		if args[0] == "delete" {
			return store.DeletePlacement(ctx, args[1])
		}
		placements, err := store.ListPlacements(ctx)
		if err != nil {
			return err
		}
		for _, placement := range placements {
			if placement.Name != args[1] {
				continue
			}
			hostfile, err := placement.Hostfile()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "# Save as hostfile.json:\n%s\n\n# Then run, from a machine that can reach every node over SSH:\n%s\n", hostfile, placement.Command("hostfile.json"))
			return err
		}
		return fmt.Errorf("no placement named %s", args[1])
	case args[0] == "list" && len(args) == 1:
		placements, err := store.ListPlacements(ctx)
		if err != nil {
			return err
		}
		for _, placement := range placements {
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", placement.Name, placement.Model, strings.Join(placement.Nodes, ",")); err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.New(clusterUsage)
	}
}
