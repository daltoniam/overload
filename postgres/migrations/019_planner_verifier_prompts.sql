-- Workflows can have a planner prompt, which assigns changed files to
-- sub-agents, and a verifier prompt, which keeps or drops sub-agent
-- findings. Both are referenced from workflows.routing.
ALTER TABLE prompt_templates DROP CONSTRAINT IF EXISTS prompt_templates_kind_check;
ALTER TABLE prompt_templates ADD CONSTRAINT prompt_templates_kind_check CHECK (kind IN ('entry', 'review', 'plan', 'verify'));
