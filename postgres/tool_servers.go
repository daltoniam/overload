package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// ErrToolServerInUse means an agent still uses a tool server being deleted.
var ErrToolServerInUse = errors.New("tool server in use")

func (s *Store) SaveToolServer(ctx context.Context, server overload.ToolServer) error {
	if err := server.Validate(); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO tool_servers(name,url,token_env,description,enabled) VALUES ($1,$2,$3,$4,$5) ON CONFLICT(name) DO UPDATE SET url=EXCLUDED.url,token_env=EXCLUDED.token_env,description=EXCLUDED.description,enabled=EXCLUDED.enabled,updated_at=now()`, server.Name, server.URL, server.TokenEnv, server.Description, server.Enabled)
	return err
}

func (s *Store) ListToolServers(ctx context.Context) ([]overload.ToolServer, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name,url,token_env,description,enabled FROM tool_servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var servers []overload.ToolServer
	for rows.Next() {
		var server overload.ToolServer
		if err := rows.Scan(&server.Name, &server.URL, &server.TokenEnv, &server.Description, &server.Enabled); err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	return servers, rows.Err()
}

func (s *Store) GetToolServer(ctx context.Context, name string) (overload.ToolServer, error) {
	var server overload.ToolServer
	err := s.Pool.QueryRow(ctx, `SELECT name,url,token_env,description,enabled FROM tool_servers WHERE name=$1`, name).Scan(&server.Name, &server.URL, &server.TokenEnv, &server.Description, &server.Enabled)
	return server, err
}

// DeleteToolServer removes a tool server no agent uses.
func (s *Store) DeleteToolServer(ctx context.Context, name string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var agent string
	err = tx.QueryRow(ctx, `SELECT name FROM agent_definitions WHERE tool_servers ? $1 ORDER BY name LIMIT 1`, name).Scan(&agent)
	if err == nil {
		return fmt.Errorf("%w: agent %s uses it", ErrToolServerInUse, agent)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	command, err := tx.Exec(ctx, `DELETE FROM tool_servers WHERE name=$1`, name)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

// checkToolServers confirms every named tool server exists.
func checkToolServers(ctx context.Context, tx pgx.Tx, names []string) error {
	for _, name := range names {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tool_servers WHERE name=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("tool server %q not found", name)
		}
	}
	return nil
}

// resolveToolServers reads an agent's enabled tool servers for a run
// snapshot. A disabled or deleted server makes the workflow unavailable
// rather than silently running the agent without it.
func resolveToolServers(ctx context.Context, q querier, data []byte) ([]overload.ToolServer, error) {
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, err
	}
	var servers []overload.ToolServer
	for _, name := range names {
		var server overload.ToolServer
		err := q.QueryRow(ctx, `SELECT name,url,token_env,description,enabled FROM tool_servers WHERE name=$1`, name).Scan(&server.Name, &server.URL, &server.TokenEnv, &server.Description, &server.Enabled)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !server.Enabled) {
			return nil, fmt.Errorf("%w: tool server %s is disabled or missing", ErrWorkflowUnavailable, name)
		}
		if err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	return servers, nil
}
