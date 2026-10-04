-- Model profiles used to carry up to four named "passes" (review focus
-- instructions). Each pass becomes a sub-agent of the profile's starter
-- workflow, so review structure lives only in workflows.
CREATE TEMP TABLE pass_conversion ON COMMIT DROP AS
SELECT m.id AS profile_id,
       m.prompt_profile,
       left('legacy-' || m.id || '-' || (pass ->> 'name'), 64) AS agent_name,
       'Review focus for ' || (pass ->> 'name') || E':\n' || (pass ->> 'instructions') AS body,
       ordinality
FROM model_profiles m,
     jsonb_array_elements(CASE WHEN jsonb_typeof(m.agents) = 'array' THEN m.agents ELSE '[]'::jsonb END) WITH ORDINALITY AS passes(pass, ordinality);

INSERT INTO prompt_templates(name, kind)
SELECT DISTINCT 'legacy-' || CASE WHEN prompt_profile = 'switchboard-go' THEN 'switchboard-go' ELSE 'context' END, 'entry' FROM pass_conversion
ON CONFLICT (name, kind) DO NOTHING;
INSERT INTO prompt_revisions(template_id, revision, body, content_sha256)
SELECT t.id, 1, 'Review the pull request for concrete actionable bugs. Return JSON with summary and findings only.', encode(sha256('Review the pull request for concrete actionable bugs. Return JSON with summary and findings only.'::bytea), 'hex')
FROM prompt_templates t
WHERE t.kind = 'entry' AND t.name IN ('legacy-context', 'legacy-switchboard-go')
  AND NOT EXISTS (SELECT 1 FROM prompt_revisions r WHERE r.template_id = t.id);

INSERT INTO agent_definitions(name, model_profile_id, entry_prompt_revision_id, enabled)
SELECT DISTINCT ON (c.profile_id) 'legacy-model-' || c.profile_id, c.profile_id, r.id, true
FROM pass_conversion c
JOIN prompt_templates t ON t.kind = 'entry' AND t.name = 'legacy-' || CASE WHEN c.prompt_profile = 'switchboard-go' THEN 'switchboard-go' ELSE 'context' END
JOIN prompt_revisions r ON r.template_id = t.id AND r.revision = 1
ON CONFLICT (name) DO NOTHING;

INSERT INTO workflows(name, kind, agent_names, enabled)
SELECT DISTINCT 'legacy-workflow-' || profile_id, 'pr_review', jsonb_build_array('legacy-model-' || profile_id), true FROM pass_conversion
ON CONFLICT (name) DO NOTHING;
INSERT INTO legacy_profile_workflows(profile_id, workflow_id)
SELECT DISTINCT c.profile_id, w.id FROM pass_conversion c JOIN workflows w ON w.name = 'legacy-workflow-' || c.profile_id
ON CONFLICT (profile_id) DO NOTHING;

INSERT INTO prompt_templates(name, kind)
SELECT agent_name, 'review' FROM pass_conversion
ON CONFLICT (name, kind) DO NOTHING;
INSERT INTO prompt_revisions(template_id, revision, body, content_sha256)
SELECT t.id, 1, c.body, encode(sha256(convert_to(c.body, 'UTF8')), 'hex')
FROM pass_conversion c JOIN prompt_templates t ON t.name = c.agent_name AND t.kind = 'review'
WHERE NOT EXISTS (SELECT 1 FROM prompt_revisions r WHERE r.template_id = t.id);

INSERT INTO agent_definitions(name, model_profile_id, entry_prompt_revision_id, review_prompt_revision_id, enabled)
SELECT c.agent_name, c.profile_id, base.entry_prompt_revision_id, review.id, true
FROM pass_conversion c
JOIN agent_definitions base ON base.name = 'legacy-model-' || c.profile_id
JOIN prompt_templates t ON t.name = c.agent_name AND t.kind = 'review'
JOIN prompt_revisions review ON review.template_id = t.id AND review.revision = 1
ON CONFLICT (name) DO NOTHING;

UPDATE workflows w
SET agent_names = w.agent_names || (
        SELECT COALESCE(jsonb_agg(to_jsonb(c.agent_name) ORDER BY c.ordinality), '[]'::jsonb)
        FROM pass_conversion c
        WHERE 'legacy-workflow-' || c.profile_id = w.name AND NOT w.agent_names ? c.agent_name),
    revision = w.revision + 1,
    updated_at = now()
WHERE w.name IN (SELECT 'legacy-workflow-' || profile_id FROM pass_conversion);

ALTER TABLE model_profiles DROP COLUMN IF EXISTS agents;
