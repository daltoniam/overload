package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

func (s *Store) SavePrompt(ctx context.Context, prompt overload.PromptTemplate) (overload.PromptTemplate, error) {
	if err := prompt.Validate(); err != nil {
		return prompt, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return prompt, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var templateID int64
	if err := tx.QueryRow(ctx, `INSERT INTO prompt_templates(name, kind) VALUES ($1,$2) ON CONFLICT(name,kind) DO UPDATE SET name=EXCLUDED.name RETURNING id`, prompt.Name, prompt.Kind).Scan(&templateID); err != nil {
		return prompt, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, templateID+801000); err != nil {
		return prompt, err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0)+1 FROM prompt_revisions WHERE template_id=$1`, templateID).Scan(&prompt.Revision); err != nil {
		return prompt, err
	}
	prompt.SHA256 = overload.PromptDigest(prompt.Body)
	if _, err := tx.Exec(ctx, `INSERT INTO prompt_revisions(template_id,revision,body,content_sha256) VALUES ($1,$2,$3,$4)`, templateID, prompt.Revision, prompt.Body, prompt.SHA256); err != nil {
		return prompt, err
	}
	return prompt, tx.Commit(ctx)
}

func (s *Store) GetPrompt(ctx context.Context, kind, name string, revision int) (overload.PromptTemplate, error) {
	var prompt overload.PromptTemplate
	query := `SELECT t.name,t.kind,r.revision,r.body,r.content_sha256 FROM prompt_templates t JOIN prompt_revisions r ON r.template_id=t.id WHERE t.kind=$1 AND t.name=$2 AND ($3=0 OR r.revision=$3) ORDER BY r.revision DESC LIMIT 1`
	err := s.Pool.QueryRow(ctx, query, kind, name, revision).Scan(&prompt.Name, &prompt.Kind, &prompt.Revision, &prompt.Body, &prompt.SHA256)
	return prompt, err
}

func (s *Store) ListPrompts(ctx context.Context) ([]overload.PromptTemplate, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (t.kind,t.name) t.name,t.kind,r.revision,r.body,r.content_sha256 FROM prompt_templates t JOIN prompt_revisions r ON r.template_id=t.id ORDER BY t.kind,t.name,r.revision DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prompts []overload.PromptTemplate
	for rows.Next() {
		var prompt overload.PromptTemplate
		if err := rows.Scan(&prompt.Name, &prompt.Kind, &prompt.Revision, &prompt.Body, &prompt.SHA256); err != nil {
			return nil, err
		}
		prompts = append(prompts, prompt)
	}
	return prompts, rows.Err()
}

func (s *Store) SaveAgent(ctx context.Context, agent overload.AgentDefinition) error {
	if err := agent.Validate(); err != nil {
		return err
	}
	if agent.Kind == "" {
		agent.Kind = "pr_review"
	}
	if _, err := s.GetPrompt(ctx, "entry", agent.EntryPrompt, 0); err != nil {
		return err
	}
	if agent.ReviewPrompt != "" {
		if _, err := s.GetPrompt(ctx, "review", agent.ReviewPrompt, 0); err != nil {
			return err
		}
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO agent_definitions(name,model_profile_id,entry_prompt_revision_id,review_prompt_revision_id,enabled,kind) VALUES ($1,(SELECT id FROM model_profiles WHERE name=$2),(SELECT r.id FROM prompt_revisions r JOIN prompt_templates t ON t.id=r.template_id WHERE t.name=$3 AND t.kind='entry' ORDER BY r.revision DESC LIMIT 1),(SELECT r.id FROM prompt_revisions r JOIN prompt_templates t ON t.id=r.template_id WHERE t.name=$4 AND t.kind='review' ORDER BY r.revision DESC LIMIT 1),$5,$6) ON CONFLICT(name) DO UPDATE SET model_profile_id=EXCLUDED.model_profile_id,entry_prompt_revision_id=EXCLUDED.entry_prompt_revision_id,review_prompt_revision_id=EXCLUDED.review_prompt_revision_id,enabled=EXCLUDED.enabled,kind=EXCLUDED.kind,updated_at=now()`, agent.Name, agent.Model, agent.EntryPrompt, agent.ReviewPrompt, agent.Enabled, agent.Kind)
	return err
}

func (s *Store) ListAgents(ctx context.Context) ([]overload.AgentDefinition, error) {
	rows, err := s.Pool.Query(ctx, `SELECT a.name,a.kind,m.name,entry.name,COALESCE(review.name,''),a.enabled FROM agent_definitions a JOIN model_profiles m ON m.id=a.model_profile_id JOIN prompt_revisions er ON er.id=a.entry_prompt_revision_id JOIN prompt_templates entry ON entry.id=er.template_id LEFT JOIN prompt_revisions rr ON rr.id=a.review_prompt_revision_id LEFT JOIN prompt_templates review ON review.id=rr.template_id ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []overload.AgentDefinition
	for rows.Next() {
		var agent overload.AgentDefinition
		if err := rows.Scan(&agent.Name, &agent.Kind, &agent.Model, &agent.EntryPrompt, &agent.ReviewPrompt, &agent.Enabled); err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func (s *Store) SaveWorkflow(ctx context.Context, workflow overload.Workflow) error {
	if err := workflow.Validate(); err != nil {
		return err
	}
	for _, name := range workflow.Agents {
		var enabled bool
		var kind string
		if err := s.Pool.QueryRow(ctx, `SELECT enabled,kind FROM agent_definitions WHERE name=$1`, name).Scan(&enabled, &kind); err != nil || !enabled || kind != workflow.Kind {
			return fmt.Errorf("workflow agent %q not available for %s", name, workflow.Kind)
		}
	}
	data, err := json.Marshal(workflow.Agents)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO workflows(name,kind,agent_names,enabled) VALUES ($1,$2,$3,$4) ON CONFLICT(name) DO UPDATE SET kind=EXCLUDED.kind,agent_names=EXCLUDED.agent_names,enabled=EXCLUDED.enabled,revision=workflows.revision+1,updated_at=now()`, workflow.Name, workflow.Kind, data, workflow.Enabled)
	return err
}

func (s *Store) ListWorkflows(ctx context.Context) ([]overload.Workflow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name,kind,revision,agent_names,enabled FROM workflows ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var workflows []overload.Workflow
	for rows.Next() {
		var workflow overload.Workflow
		var data []byte
		if err := rows.Scan(&workflow.Name, &workflow.Kind, &workflow.Revision, &data, &workflow.Enabled); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &workflow.Agents); err != nil {
			return nil, err
		}
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
	var result overload.ResolvedWorkflow
	var names []byte
	err := q.QueryRow(ctx, `SELECT name,kind,revision,agent_names FROM workflows WHERE name=$1 AND enabled`, name).Scan(&result.Name, &result.Kind, &result.Revision, &names)
	if err != nil {
		return result, err
	}
	var agents []string
	if err := json.Unmarshal(names, &agents); err != nil {
		return result, err
	}
	for _, agentName := range agents {
		var agent overload.ResolvedAgent
		var entryID int64
		var reviewID *int64
		agent.Name = agentName
		err := q.QueryRow(ctx, `SELECT m.name,m.provider,m.connection_kind,m.base_url,m.model,m.api_key_env,m.concurrency,m.reasoning_param,m.reasoning_effort,m.max_output_tokens,a.entry_prompt_revision_id,a.review_prompt_revision_id FROM agent_definitions a JOIN model_profiles m ON m.id=a.model_profile_id WHERE a.name=$1 AND a.enabled`, agentName).Scan(&agent.Model.Name, &agent.Model.Provider, &agent.Model.ConnectionKind, &agent.Model.BaseURL, &agent.Model.Model, &agent.Model.APIKeyEnv, &agent.Model.Concurrency, &agent.Model.ReasoningParam, &agent.Model.ReasoningEffort, &agent.Model.MaxOutputTokens, &entryID, &reviewID)
		if err != nil {
			return result, err
		}
		if err := loadPromptRevision(ctx, q, entryID, &agent.EntryPrompt); err != nil {
			return result, err
		}
		if reviewID != nil {
			if err := loadPromptRevision(ctx, q, *reviewID, &agent.ReviewPrompt); err != nil {
				return result, err
			}
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
	if name == "" || len(name) > 200 {
		return errors.New("invalid repository name")
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
