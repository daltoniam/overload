package postgres

import (
	"context"
	"encoding/json"

	"github.com/daltoniam/overload"
)

// ReadRunRouting returns which agent reviewed which files in a finished PR
// review run, with per-agent findings and tokens. Runs that have not
// finished, or finished before routing was recorded, return an empty
// routing.
func (s *Store) ReadRunRouting(ctx context.Context, runID int64) (overload.Routing, error) {
	var routing overload.Routing
	var data []byte
	if err := s.Pool.QueryRow(ctx, `SELECT metrics->'routing' FROM runs WHERE id=$1`, runID).Scan(&data); err != nil {
		return routing, err
	}
	if len(data) == 0 || string(data) == "null" {
		return routing, nil
	}
	return routing, json.Unmarshal(data, &routing)
}
