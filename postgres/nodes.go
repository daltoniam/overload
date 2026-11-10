package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// MaxNodes bounds the inventory; reports from more machines are refused.
const MaxNodes = 500

// MaxNodesPerKey bounds the names one API key may claim, so one leaked key
// cannot fill the inventory.
const MaxNodesPerKey = 100

// nodeLock serializes claims of new or released names so the caps hold
// under concurrent reports.
const nodeLock = 729140

// validKey is true when the joined api_keys row k can still claim nodes.
const validKey = `(k.id IS NOT NULL AND k.revoked_at IS NULL AND k.expires_at > now())`

// ReportNode stores a node's latest report from the API key keyID. The
// first key to report a name claims it; reports for that name from any
// other key are refused while the owning key is valid. A revoked or
// expired key's names, and released names, go to the next key to report.
func (s *Store) ReportNode(ctx context.Context, keyID int64, report overload.NodeReport) error {
	if err := report.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	// The usual report comes from the key that owns the name and needs no
	// lock.
	command, err := s.Pool.Exec(ctx, `UPDATE nodes SET report = $2, last_seen = now() WHERE name = $1 AND api_key_id = $3`, report.Name, data, keyID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, nodeLock); err != nil {
		return err
	}
	var owner *int64
	var ownerValid bool
	err = tx.QueryRow(ctx, `SELECT n.api_key_id, `+validKey+` FROM nodes n LEFT JOIN api_keys k ON k.id = n.api_key_id WHERE n.name = $1 FOR UPDATE OF n`, report.Name).Scan(&owner, &ownerValid)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		var total, owned int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE api_key_id = $1) FROM nodes`, keyID).Scan(&total, &owned); err != nil {
			return err
		}
		if total >= MaxNodes {
			return fmt.Errorf("%w: the inventory already holds %d nodes; forget nodes that are gone", overload.ErrTooManyNodes, MaxNodes)
		}
		if owned >= MaxNodesPerKey {
			return fmt.Errorf("%w: this API key already reports %d nodes; use another key", overload.ErrTooManyNodes, MaxNodesPerKey)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO nodes (name, report, api_key_id) VALUES ($1, $2, $3)`, report.Name, data, keyID); err != nil {
			return err
		}
	case err != nil:
		return err
	case owner != nil && *owner != keyID && ownerValid:
		return overload.ErrNodeClaimed
	default:
		if _, err := tx.Exec(ctx, `UPDATE nodes SET report = $2, api_key_id = $3, last_seen = now() WHERE name = $1`, report.Name, data, keyID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ListNodes returns the inventory by name, with the prefix of the key that
// holds each name while that key is valid.
func (s *Store) ListNodes(ctx context.Context) ([]overload.Node, error) {
	rows, err := s.Pool.Query(ctx, `SELECT n.report, n.last_seen, CASE WHEN `+validKey+` THEN k.prefix ELSE '' END FROM nodes n LEFT JOIN api_keys k ON k.id = n.api_key_id ORDER BY n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []overload.Node
	for rows.Next() {
		var node overload.Node
		var data []byte
		if err := rows.Scan(&data, &node.LastSeen, &node.KeyPrefix); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &node.Report); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// ReleaseNode frees a node's name for the next key that reports it,
// keeping the node and any placement that uses it, so a node can move to a
// new key without being forgotten.
func (s *Store) ReleaseNode(ctx context.Context, name string) error {
	command, err := s.Pool.Exec(ctx, `UPDATE nodes SET api_key_id = NULL WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DeleteNode forgets a node and releases its name for any key; it
// reappears if it reports again. A node a placement names is kept, since
// the placement's launch configuration still uses it.
func (s *Store) DeleteNode(ctx context.Context, name string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Locking the row first makes a placement saved at the same time,
	// which share-locks its nodes, visible to the check below.
	if err := tx.QueryRow(ctx, `SELECT name FROM nodes WHERE name = $1 FOR UPDATE`, name).Scan(&name); err != nil {
		return err
	}
	var placement string
	err = tx.QueryRow(ctx, `SELECT name FROM placements WHERE spec->'nodes' ? $1 ORDER BY name LIMIT 1`, name).Scan(&placement)
	if err == nil {
		return fmt.Errorf("%w: placement %s uses %s; delete the placement first", overload.ErrNodeInPlacement, placement, name)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM nodes WHERE name = $1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SavePlacement validates and stores a placement; every node must be in
// the inventory.
func (s *Store) SavePlacement(ctx context.Context, placement overload.Placement) error {
	if err := placement.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(placement)
	if err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT name FROM nodes WHERE name = ANY($1) FOR SHARE`, placement.Nodes)
	if err != nil {
		return err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, node := range placement.Nodes {
		if !slices.Contains(found, node) {
			return fmt.Errorf("node %s has not reported; run overload node report on it first", node)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO placements (name, spec) VALUES ($1, $2) ON CONFLICT (name) DO UPDATE SET spec = EXCLUDED.spec, updated_at = now()`, placement.Name, data); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
