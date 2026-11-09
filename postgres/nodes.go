package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// MaxNodes bounds the inventory; reports from more machines are refused.
const MaxNodes = 500

// ReportNode stores a node's latest report.
func (s *Store) ReportNode(ctx context.Context, report overload.NodeReport) error {
	if err := report.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	command, err := s.Pool.Exec(ctx, `INSERT INTO nodes (name, report) SELECT $1, $2 WHERE EXISTS (SELECT 1 FROM nodes WHERE name = $1) OR (SELECT count(*) FROM nodes) < $3
		ON CONFLICT (name) DO UPDATE SET report = EXCLUDED.report, last_seen = now()`, report.Name, data, MaxNodes)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return fmt.Errorf("the inventory already holds %d nodes", MaxNodes)
	}
	return nil
}

// ListNodes returns the inventory by name.
func (s *Store) ListNodes(ctx context.Context) ([]overload.Node, error) {
	rows, err := s.Pool.Query(ctx, `SELECT report, last_seen FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []overload.Node
	for rows.Next() {
		var node overload.Node
		var data []byte
		if err := rows.Scan(&data, &node.LastSeen); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &node.Report); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// DeleteNode forgets a node; it reappears if it reports again.
func (s *Store) DeleteNode(ctx context.Context, name string) error {
	command, err := s.Pool.Exec(ctx, `DELETE FROM nodes WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SavePlacement validates and stores a placement; every node must be in
// the inventory.
func (s *Store) SavePlacement(ctx context.Context, placement overload.Placement) error {
	if err := placement.Validate(); err != nil {
		return err
	}
	for _, node := range placement.Nodes {
		var exists bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM nodes WHERE name = $1)`, node).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("node %s has not reported; run overload node report on it first", node)
		}
	}
	data, err := json.Marshal(placement)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO placements (name, spec) VALUES ($1, $2) ON CONFLICT (name) DO UPDATE SET spec = EXCLUDED.spec, updated_at = now()`, placement.Name, data)
	return err
}

// ListPlacements returns the placements by name.
func (s *Store) ListPlacements(ctx context.Context) ([]overload.Placement, error) {
	rows, err := s.Pool.Query(ctx, `SELECT spec FROM placements ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var placements []overload.Placement
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var placement overload.Placement
		if err := json.Unmarshal(data, &placement); err != nil {
			return nil, err
		}
		placements = append(placements, placement)
	}
	return placements, rows.Err()
}

// DeletePlacement deletes a placement.
func (s *Store) DeletePlacement(ctx context.Context, name string) error {
	command, err := s.Pool.Exec(ctx, `DELETE FROM placements WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
