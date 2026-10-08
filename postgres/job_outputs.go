package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

type JobOutput struct {
	Kind   string
	Agents []AgentOutput
}

type AgentOutput struct {
	Name string `json:"name"`
	Text string `json:"text"`
	// Steps, ToolCalls and the token counts describe tool-using agents.
	Steps        int   `json:"steps,omitempty"`
	ToolCalls    int   `json:"tool_calls,omitempty"`
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	HitStepLimit bool  `json:"hit_step_limit,omitempty"`
}

func (s *Store) ReadJobOutput(ctx context.Context, runID int64) (JobOutput, error) {
	var output JobOutput
	var content []byte
	err := s.Pool.QueryRow(ctx, `SELECT kind, content FROM job_outputs WHERE run_id=$1 AND kind='scheduled_prompt'`, runID).Scan(&output.Kind, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		return output, nil
	}
	if err != nil {
		return output, err
	}
	if len(content) > 200000 {
		return JobOutput{}, errors.New("job output exceeds display limit")
	}
	if err := json.Unmarshal(content, &output.Agents); err != nil {
		return JobOutput{}, err
	}
	return output, nil
}
