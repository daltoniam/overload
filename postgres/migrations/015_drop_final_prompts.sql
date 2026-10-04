UPDATE agent_definitions SET final_prompt_revision_id = NULL WHERE final_prompt_revision_id IS NOT NULL;
ALTER TABLE agent_definitions DROP COLUMN IF EXISTS final_prompt_revision_id;
DELETE FROM prompt_revisions WHERE template_id IN (SELECT id FROM prompt_templates WHERE kind = 'final');
DELETE FROM prompt_templates WHERE kind = 'final';
ALTER TABLE prompt_templates DROP CONSTRAINT IF EXISTS prompt_templates_kind_check;
ALTER TABLE prompt_templates ADD CONSTRAINT prompt_templates_kind_check CHECK (kind IN ('entry', 'review'));
