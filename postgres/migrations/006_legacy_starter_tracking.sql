CREATE TABLE IF NOT EXISTS legacy_profile_workflows (
    profile_id bigint PRIMARY KEY REFERENCES model_profiles(id) ON DELETE CASCADE,
    workflow_id bigint NOT NULL UNIQUE REFERENCES workflows(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO prompt_templates(name,kind)
SELECT DISTINCT 'legacy-' || prompt_profile, 'entry' FROM model_profiles
WHERE prompt_profile IN ('context','switchboard-go')
ON CONFLICT(name,kind) DO NOTHING;
INSERT INTO prompt_revisions(template_id,revision,body,content_sha256)
SELECT t.id,1,'Review the pull request for concrete actionable bugs. Return JSON with summary and findings only.',encode(sha256('Review the pull request for concrete actionable bugs. Return JSON with summary and findings only.'::bytea),'hex')
FROM prompt_templates t WHERE t.name LIKE 'legacy-%' AND t.kind='entry'
AND NOT EXISTS(SELECT 1 FROM prompt_revisions r WHERE r.template_id=t.id);
INSERT INTO agent_definitions(name,model_profile_id,entry_prompt_revision_id,enabled)
SELECT 'legacy-model-' || m.id,m.id,r.id,true
FROM model_profiles m JOIN prompt_templates t ON t.name='legacy-' || m.prompt_profile AND t.kind='entry'
JOIN prompt_revisions r ON r.template_id=t.id AND r.revision=1
WHERE m.prompt_profile IN ('context','switchboard-go')
ON CONFLICT(name) DO NOTHING;
INSERT INTO workflows(name,kind,agent_names,enabled)
SELECT 'legacy-workflow-' || m.id,'pr_review',jsonb_build_array('legacy-model-' || m.id),true
FROM model_profiles m WHERE m.prompt_profile IN ('context','switchboard-go')
ON CONFLICT(name) DO NOTHING;
INSERT INTO legacy_profile_workflows(profile_id,workflow_id)
SELECT m.id,w.id FROM model_profiles m JOIN workflows w ON w.name='legacy-workflow-' || m.id
ON CONFLICT(profile_id) DO NOTHING;
