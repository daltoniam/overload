package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// Owned prompt names. Agents and workflows own their prompts; the names use a
// ':' that user-chosen names cannot contain, so they never collide.
func agentPromptName(agent string) string       { return "agent:" + agent }
func workflowPromptName(workflow string) string { return "workflow:" + workflow }

// savePromptText stores text as the latest revision of an owned prompt,
// adding a revision only when the text changed, and returns that
// revision's ID.
func savePromptText(ctx context.Context, tx pgx.Tx, name, kind, text string) (int64, error) {
	var templateID int64
	if err := tx.QueryRow(ctx, `INSERT INTO prompt_templates(name, kind) VALUES ($1,$2) ON CONFLICT(name,kind) DO UPDATE SET name=EXCLUDED.name RETURNING id`, name, kind).Scan(&templateID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, templateID+801000); err != nil {
		return 0, err
	}
	var id int64
	var revision int
	var body string
	err := tx.QueryRow(ctx, `SELECT id,revision,body FROM prompt_revisions WHERE template_id=$1 ORDER BY revision DESC LIMIT 1`, templateID).Scan(&id, &revision, &body)
	if err == nil && body == text {
		return id, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO prompt_revisions(template_id,revision,body,content_sha256) VALUES ($1,$2,$3,$4) RETURNING id`, templateID, revision+1, text, overload.PromptDigest(text)).Scan(&id)
	return id, err
}

// deleteOwnedPrompts removes an agent's or workflow's prompts once nothing
// references them. Runs keep their prompts in their pinned snapshots.
func deleteOwnedPrompts(ctx context.Context, tx pgx.Tx, name string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE name=$1)`, name); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM prompt_templates WHERE name=$1`, name)
	return err
}

// ErrConflict means the agent or workflow changed since the caller read it,
// so saving would overwrite someone else's edit.
var ErrConflict = errors.New("changed since it was loaded")

// checkRevision locks the row query selects and compares its revision with
// expected. An expected revision of 0 skips the check (a new resource, or a
// configuration file that does not name one).
func checkRevision(ctx context.Context, tx pgx.Tx, query, name string, expected int) error {
	if expected == 0 {
		return nil
	}
	var current int
	err := tx.QueryRow(ctx, query, name).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s was deleted", ErrConflict, name)
	}
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("%w: %s is at version %d, not %d", ErrConflict, name, current, expected)
	}
	return nil
}

// ErrAgentInUse means an agent change would break a workflow that uses it.
var ErrAgentInUse = errors.New("agent in use")

// ErrWorkflowUnavailable means a workflow cannot run as configured, for
// example because one of its agents is disabled.
var ErrWorkflowUnavailable = errors.New("workflow unavailable")

// SaveAgent saves an agent with its prompt. Changed prompt text becomes a new
// revision that the agent uses from then on. An agent's job type cannot be
// changed while a workflow of the old type uses it. A non-zero
// PromptRevision must match the stored one, so an edit made from an older
// copy of the instructions is refused instead of overwriting newer ones.
func (s *Store) SaveAgent(ctx context.Context, agent overload.AgentDefinition) error {
	agent.Prompt = overload.NormalizePrompt(agent.Prompt)
	if err := agent.Validate(); err != nil {
		return err
	}
	if agent.Kind == "" {
		agent.Kind = "pr_review"
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := checkRevision(ctx, tx, `SELECT r.revision FROM agent_definitions a JOIN prompt_revisions r ON r.id=a.entry_prompt_revision_id WHERE a.name=$1 FOR UPDATE OF a`, agent.Name, agent.PromptRevision); err != nil {
		return err
	}
	var modelID int64
	if err := tx.QueryRow(ctx, `SELECT id FROM model_profiles WHERE name=$1`, agent.Model).Scan(&modelID); err != nil {
		return fmt.Errorf("model connection %q not found", agent.Model)
	}
	if err := checkToolServers(ctx, tx, agent.Tools); err != nil {
		return err
	}
	tools := agent.Tools
	if tools == nil {
		tools = []string{}
	}
	toolData, err := json.Marshal(tools)
	if err != nil {
		return err
	}
	var conflicting string
	err = tx.QueryRow(ctx, `SELECT w.name FROM workflows w WHERE w.agent_names ? $1 AND w.kind <> $2 ORDER BY w.name LIMIT 1`, agent.Name, agent.Kind).Scan(&conflicting)
	if err == nil {
		return fmt.Errorf("%w: workflow %s uses agent %s, so its job type cannot change", ErrAgentInUse, conflicting, agent.Name)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	revisionID, err := savePromptText(ctx, tx, agentPromptName(agent.Name), overload.PromptEntry, agent.Prompt)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_definitions(name,model_profile_id,entry_prompt_revision_id,enabled,kind,tool_servers) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT(name) DO UPDATE SET model_profile_id=EXCLUDED.model_profile_id,entry_prompt_revision_id=EXCLUDED.entry_prompt_revision_id,enabled=EXCLUDED.enabled,kind=EXCLUDED.kind,tool_servers=EXCLUDED.tool_servers,updated_at=now()`, agent.Name, modelID, revisionID, agent.Enabled, agent.Kind, toolData); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListAgents(ctx context.Context) ([]overload.AgentDefinition, error) {
	rows, err := s.Pool.Query(ctx, `SELECT a.name,a.kind,m.name,r.body,r.revision,a.enabled,a.tool_servers FROM agent_definitions a JOIN model_profiles m ON m.id=a.model_profile_id JOIN prompt_revisions r ON r.id=a.entry_prompt_revision_id ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []overload.AgentDefinition
	for rows.Next() {
		var agent overload.AgentDefinition
		var tools []byte
		if err := rows.Scan(&agent.Name, &agent.Kind, &agent.Model, &agent.Prompt, &agent.PromptRevision, &agent.Enabled, &tools); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(tools, &agent.Tools); err != nil {
			return nil, err
		}
		if len(agent.Tools) == 0 {
			agent.Tools = nil
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// SaveWorkflow saves a workflow with its planner and verifier prompts.
// Changed prompt text becomes a new revision.
func (s *Store) SaveWorkflow(ctx context.Context, workflow overload.Workflow) error {
	workflow.Normalize()
	if err := workflow.Validate(); err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := checkRevision(ctx, tx, `SELECT revision FROM workflows WHERE name=$1 FOR UPDATE`, workflow.Name, workflow.Revision); err != nil {
		return err
	}
	for _, name := range workflow.Agents {
		var enabled bool
		var kind string
		if err := tx.QueryRow(ctx, `SELECT enabled,kind FROM agent_definitions WHERE name=$1 FOR SHARE`, name).Scan(&enabled, &kind); err != nil || !enabled || kind != workflow.Kind {
			return fmt.Errorf("workflow agent %q not available for %s", name, workflow.Kind)
		}
	}
	data, err := json.Marshal(workflow.Agents)
	if err != nil {
		return err
	}
	stored := workflowRouting{Scopes: workflow.Scopes, SkipPaths: workflow.SkipPaths, MainReviews: workflow.MainReviews, MaxFileReviews: workflow.MaxFileReviews, MaxFindings: workflow.MaxFindings, MaxSteps: workflow.MaxSteps, TimeoutMinutes: workflow.TimeoutMinutes, ReviewDecision: workflow.ReviewDecision, BlockSeverity: workflow.BlockSeverity}
	for _, prompt := range []struct {
		text, kind string
		id         *int64
	}{{workflow.PlannerPrompt, overload.PromptPlan, &stored.PlannerRevisionID}, {workflow.VerifierPrompt, overload.PromptVerify, &stored.VerifierRevisionID}} {
		if prompt.text == "" {
			continue
		}
		if *prompt.id, err = savePromptText(ctx, tx, workflowPromptName(workflow.Name), prompt.kind, prompt.text); err != nil {
			return err
		}
	}
	routing, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflows(name,kind,agent_names,enabled,routing) VALUES ($1,$2,$3,$4,$5) ON CONFLICT(name) DO UPDATE SET kind=EXCLUDED.kind,agent_names=EXCLUDED.agent_names,enabled=EXCLUDED.enabled,routing=EXCLUDED.routing,revision=workflows.revision+1,updated_at=now() WHERE (workflows.kind,workflows.agent_names,workflows.enabled,workflows.routing) IS DISTINCT FROM (EXCLUDED.kind,EXCLUDED.agent_names,EXCLUDED.enabled,EXCLUDED.routing)`, workflow.Name, workflow.Kind, data, workflow.Enabled, routing); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// workflowRouting is the stored form of a workflow's skip paths, sub-agent
// scopes, main agent review mode, file-review limit and the revisions of its
// planner and verifier prompts.
type workflowRouting struct {
	Scopes             map[string]overload.Scope `json:"scopes,omitempty"`
	SkipPaths          []string                  `json:"skip_paths,omitempty"`
	MainReviews        string                    `json:"main_reviews,omitempty"`
	MaxFileReviews     int                       `json:"max_file_reviews,omitempty"`
	MaxFindings        int                       `json:"max_findings,omitempty"`
	PlannerRevisionID  int64                     `json:"planner_prompt_revision_id,omitempty"`
	VerifierRevisionID int64                     `json:"verifier_prompt_revision_id,omitempty"`
	MaxSteps           int                       `json:"max_steps,omitempty"`
	TimeoutMinutes     int                       `json:"timeout_minutes,omitempty"`
	ReviewDecision     string                    `json:"review_decision,omitempty"`
	BlockSeverity      string                    `json:"block_severity,omitempty"`
}

func (s *Store) ListWorkflows(ctx context.Context) ([]overload.Workflow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT w.name,w.kind,w.revision,w.agent_names,w.enabled,w.routing,COALESCE(p.body,''),COALESCE(v.body,'') FROM workflows w LEFT JOIN prompt_revisions p ON p.id=(w.routing->>'planner_prompt_revision_id')::bigint LEFT JOIN prompt_revisions v ON v.id=(w.routing->>'verifier_prompt_revision_id')::bigint ORDER BY w.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var workflows []overload.Workflow
	for rows.Next() {
		var workflow overload.Workflow
		var data, routingData []byte
		if err := rows.Scan(&workflow.Name, &workflow.Kind, &workflow.Revision, &data, &workflow.Enabled, &routingData, &workflow.PlannerPrompt, &workflow.VerifierPrompt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &workflow.Agents); err != nil {
			return nil, err
		}
		var routing workflowRouting
		if err := json.Unmarshal(routingData, &routing); err != nil {
			return nil, err
		}
		workflow.Scopes, workflow.SkipPaths, workflow.MainReviews, workflow.MaxFileReviews, workflow.MaxFindings = routing.Scopes, routing.SkipPaths, routing.MainReviews, routing.MaxFileReviews, routing.MaxFindings
		workflow.MaxSteps, workflow.TimeoutMinutes = routing.MaxSteps, routing.TimeoutMinutes
		workflow.ReviewDecision, workflow.BlockSeverity = routing.ReviewDecision, routing.BlockSeverity
		workflows = append(workflows, workflow)
	}
	return workflows, rows.Err()
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) ResolveWorkflow(ctx context.Context, name string) (overload.ResolvedWorkflow, error) {
	return resolveWorkflow(ctx, s.Pool, name)
}

// resolveWorkflow reads a workflow and the current revisions of its agents'
// prompts and models through q, so callers inside a transaction pin a
// consistent snapshot.
func resolveWorkflow(ctx context.Context, q querier, name string) (overload.ResolvedWorkflow, error) {
	result := overload.ResolvedWorkflow{Version: overload.SnapshotVersion}
	var names, routingData []byte
	err := q.QueryRow(ctx, `SELECT name,kind,revision,agent_names,routing FROM workflows WHERE name=$1 AND enabled`, name).Scan(&result.Name, &result.Kind, &result.Revision, &names, &routingData)
	if err != nil {
		return result, err
	}
	var agents []string
	if err := json.Unmarshal(names, &agents); err != nil {
		return result, err
	}
	var routing workflowRouting
	if err := json.Unmarshal(routingData, &routing); err != nil {
		return result, err
	}
	result.SkipPaths, result.MainReviews, result.MaxFileReviews, result.MaxFindings = routing.SkipPaths, routing.MainReviews, routing.MaxFileReviews, routing.MaxFindings
	result.MaxSteps, result.TimeoutMinutes = routing.MaxSteps, routing.TimeoutMinutes
	result.ReviewDecision, result.BlockSeverity = routing.ReviewDecision, routing.BlockSeverity
	for id, target := range map[int64]**overload.PromptTemplate{routing.PlannerRevisionID: &result.PlannerPrompt, routing.VerifierRevisionID: &result.VerifierPrompt} {
		if id == 0 {
			continue
		}
		var prompt overload.PromptTemplate
		if err := loadPromptRevision(ctx, q, id, &prompt); err != nil {
			return result, err
		}
		*target = &prompt
	}
	for _, agentName := range agents {
		var agent overload.ResolvedAgent
		var entryID int64
		var tools []byte
		agent.Name = agentName
		agent.Scope = routing.Scopes[agentName]
		err := q.QueryRow(ctx, `SELECT m.name,m.provider,m.connection_kind,m.base_url,m.model,m.api_key_env,m.concurrency,m.reasoning_param,m.reasoning_effort,m.max_output_tokens,m.api,a.entry_prompt_revision_id,a.tool_servers FROM agent_definitions a JOIN model_profiles m ON m.id=a.model_profile_id WHERE a.name=$1 AND a.enabled AND a.kind=$2`, agentName, result.Kind).Scan(&agent.Model.Name, &agent.Model.Provider, &agent.Model.ConnectionKind, &agent.Model.BaseURL, &agent.Model.Model, &agent.Model.APIKeyEnv, &agent.Model.Concurrency, &agent.Model.ReasoningParam, &agent.Model.ReasoningEffort, &agent.Model.MaxOutputTokens, &agent.Model.API, &entryID, &tools)
		if errors.Is(err, pgx.ErrNoRows) {
			return result, fmt.Errorf("%w: agent %s is disabled, missing or for a different job type", ErrWorkflowUnavailable, agentName)
		}
		if err != nil {
			return result, err
		}
		if err := loadPromptRevision(ctx, q, entryID, &agent.EntryPrompt); err != nil {
			return result, err
		}
		if agent.Tools, err = resolveToolServers(ctx, q, tools); err != nil {
			return result, err
		}
		result.Agents = append(result.Agents, agent)
	}
	if len(result.Agents) == 0 {
		return result, errors.New("workflow has no agents")
	}
	return result, nil
}

func loadPromptRevision(ctx context.Context, q querier, id int64, prompt *overload.PromptTemplate) error {
	return q.QueryRow(ctx, `SELECT t.name,t.kind,r.revision,r.body,r.content_sha256 FROM prompt_revisions r JOIN prompt_templates t ON t.id=r.template_id WHERE r.id=$1`, id).Scan(&prompt.Name, &prompt.Kind, &prompt.Revision, &prompt.Body, &prompt.SHA256)
}

func (s *Store) SaveBinding(ctx context.Context, binding overload.TriggerBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	var kind string
	if err := s.Pool.QueryRow(ctx, `SELECT kind FROM workflows WHERE name=$1 AND enabled`, binding.Workflow).Scan(&kind); err != nil {
		return err
	}
	if binding.Source != "github" || binding.Event != "pull_request" || kind != "pr_review" || (binding.Action != "opened" && binding.Action != "synchronize" && binding.Action != "reopened" && binding.Action != "ready_for_review") {
		return errors.New("only GitHub PR bindings are available")
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO trigger_bindings(source,event,action,repository_full_name,workflow_id,enabled) VALUES ($1,$2,$3,$4,(SELECT id FROM workflows WHERE name=$5),$6) ON CONFLICT(source,event,action,repository_full_name) DO UPDATE SET workflow_id=EXCLUDED.workflow_id,enabled=EXCLUDED.enabled`, binding.Source, binding.Event, binding.Action, binding.Repository, binding.Workflow, binding.Enabled)
	return err
}

func (s *Store) ListBindings(ctx context.Context) ([]overload.TriggerBinding, error) {
	rows, err := s.Pool.Query(ctx, `SELECT b.id,b.source,b.event,b.action,b.repository_full_name,w.name,b.enabled FROM trigger_bindings b JOIN workflows w ON w.id=b.workflow_id ORDER BY b.source,b.event,b.action,b.repository_full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []overload.TriggerBinding
	for rows.Next() {
		var binding overload.TriggerBinding
		if err := rows.Scan(&binding.ID, &binding.Source, &binding.Event, &binding.Action, &binding.Repository, &binding.Workflow, &binding.Enabled); err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

func (s *Store) SaveRepository(ctx context.Context, name string, enabled, dryRun bool) error {
	if !overload.ValidRepositoryName(name) {
		return errors.New("invalid repository name: use owner/name")
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO repositories(full_name,enabled,dry_run) VALUES ($1,$2,$3) ON CONFLICT(full_name) DO UPDATE SET enabled=EXCLUDED.enabled,dry_run=EXCLUDED.dry_run,updated_at=now()`, name, enabled, dryRun)
	return err
}

func (s *Store) ListRepositories(ctx context.Context) ([]overload.Repository, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,COALESCE(installation_id,0),full_name,enabled,dry_run FROM repositories ORDER BY full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repos []overload.Repository
	for rows.Next() {
		var repo overload.Repository
		if err := rows.Scan(&repo.ID, &repo.InstallationID, &repo.FullName, &repo.Enabled, &repo.DryRun); err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}
