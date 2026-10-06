package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

func (s *Store) SeedLocalDemo(ctx context.Context) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(729142)`); err != nil {
		return false, err
	}
	var existing int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM model_profiles WHERE name='demo-local-model') + (SELECT count(*) FROM prompt_templates WHERE name IN ('agent:demo-pr-reviewer','agent:demo-summary-agent')) + (SELECT count(*) FROM agent_definitions WHERE name IN ('demo-pr-reviewer','demo-summary-agent')) + (SELECT count(*) FROM workflows WHERE name IN ('demo-pr-workflow','demo-scheduled-workflow')) + (SELECT count(*) FROM repositories WHERE full_name IN ('demo/alpha','demo/beta')) + (SELECT count(*) FROM schedules WHERE name='demo-daily-summary') + (SELECT count(*) FROM trigger_bindings WHERE repository_full_name IN ('demo/alpha','demo/beta'))`).Scan(&existing); err != nil {
		return false, err
	}
	if existing != 0 {
		var complete bool
		if existing == 12 {
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_profiles WHERE name='demo-local-model' AND base_url='http://127.0.0.1:8080/v1' AND model='demo-model') AND (SELECT count(*) FROM prompt_revisions r JOIN prompt_templates p ON p.id=r.template_id WHERE p.name IN ('agent:demo-pr-reviewer','agent:demo-summary-agent') AND p.kind='entry')=2 AND EXISTS(SELECT 1 FROM agent_definitions WHERE name='demo-pr-reviewer' AND kind='pr_review') AND EXISTS(SELECT 1 FROM agent_definitions WHERE name='demo-summary-agent' AND kind='scheduled_prompt') AND EXISTS(SELECT 1 FROM schedules WHERE name='demo-daily-summary' AND NOT enabled) AND (SELECT count(*) FROM repositories WHERE full_name IN ('demo/alpha','demo/beta') AND NOT enabled AND dry_run)=2 AND (SELECT count(*) FROM trigger_bindings WHERE repository_full_name IN ('demo/alpha','demo/beta'))=2 AND EXISTS(SELECT 1 FROM runs WHERE trigger='demo_seed' AND schedule_id=(SELECT id FROM schedules WHERE name='demo-daily-summary') AND status='completed') AND EXISTS(SELECT 1 FROM job_outputs WHERE run_id IN (SELECT id FROM runs WHERE trigger='demo_seed' AND schedule_id=(SELECT id FROM schedules WHERE name='demo-daily-summary')))`).Scan(&complete)
			if err != nil {
				return false, err
			}
		}
		if complete {
			return false, nil
		}
		return false, errors.New("demo seed names already exist; refusing to change existing configuration")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_profiles(name,provider,base_url,model,prompt_profile,is_default) VALUES ('demo-local-model','openaicompat','http://127.0.0.1:8080/v1','demo-model','context',false)`); err != nil {
		return false, err
	}
	for _, agent := range []struct{ name, prompt, kind string }{
		{"demo-pr-reviewer", "Review this pull request for concrete bugs. Return only actionable findings.\n\nFocus on authorization checks and unsafe data handling.", "pr_review"},
		{"demo-summary-agent", "Summarize the supplied status data concisely.", "scheduled_prompt"},
	} {
		revisionID, err := savePromptText(ctx, tx, agentPromptName(agent.name), overload.PromptEntry, agent.prompt)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_definitions(name,model_profile_id,entry_prompt_revision_id,enabled,kind) VALUES ($1,(SELECT id FROM model_profiles WHERE name='demo-local-model'),$2,true,$3)`, agent.name, revisionID, agent.kind); err != nil {
			return false, err
		}
	}
	for _, workflow := range []struct{ name, kind, agents string }{
		{"demo-pr-workflow", "pr_review", `["demo-pr-reviewer"]`},
		{"demo-scheduled-workflow", "scheduled_prompt", `["demo-summary-agent"]`},
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO workflows(name,kind,agent_names,enabled) VALUES ($1,$2,$3,true)`, workflow.name, workflow.kind, workflow.agents); err != nil {
			return false, err
		}
	}
	for _, repo := range []string{"demo/alpha", "demo/beta"} {
		if _, err := tx.Exec(ctx, `INSERT INTO repositories(full_name,enabled,dry_run) VALUES ($1,false,true)`, repo); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO trigger_bindings(source,event,action,repository_full_name,workflow_id,enabled) VALUES ('github','pull_request','opened',$1,(SELECT id FROM workflows WHERE name='demo-pr-workflow'),true)`, repo); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schedules(name,workflow_id,cron,timezone,input_json,next_run_at,enabled) VALUES ('demo-daily-summary',(SELECT id FROM workflows WHERE name='demo-scheduled-workflow'),'0 9 * * *','UTC','{"topic":"example status"}',now()+interval '1 day',false)`); err != nil {
		return false, err
	}
	var runID int64
	if err := tx.QueryRow(ctx, `INSERT INTO runs(kind,trigger,status,dry_run,config_snapshot,input_snapshot,schedule_id,scheduled_for,started_at,finished_at) VALUES ('scheduled_prompt','demo_seed','completed',true,'{}'::jsonb,'{"topic":"example status"}'::jsonb,(SELECT id FROM schedules WHERE name='demo-daily-summary'),now(),now(),now()) RETURNING id`).Scan(&runID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_outputs(run_id,kind,content) VALUES ($1,'scheduled_prompt',$2)`, runID, `[ {"name":"demo-summary-agent","text":"Example output from a development fixture. No model was called."} ]`); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events(run_id,level,step,message) VALUES ($1,'info','completed','Development fixture: no model called')`, runID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func IsLocalDatabase(dsn string) error {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("invalid DATABASE_URL: %w", err)
	}
	host := strings.Trim(config.Host, "[]")
	if host != "localhost" {
		address := net.ParseIP(host)
		if address == nil || !address.IsLoopback() {
			return errors.New("dev seed requires a loopback PostgreSQL host")
		}
	}
	return nil
}
