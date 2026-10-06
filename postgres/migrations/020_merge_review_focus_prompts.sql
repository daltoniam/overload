-- Agents have one prompt. An agent that had a review focus prompt gets a new
-- entry prompt holding both texts, joined exactly as reviews joined them
-- (a blank line for PR reviews, a newline for scheduled prompts), so its
-- behaviour does not change. Focus areas are now separate sub-agents.
-- Review prompts are kept as entry prompts, so no text is lost; runs already
-- queued carry their prompts in their pinned snapshot.
DO $$
DECLARE
    agent record;
    candidate text;
    new_template bigint;
    new_revision bigint;
    merged text;
BEGIN
    FOR agent IN
        SELECT a.id, a.name, a.kind, er.body AS entry_body, rr.body AS review_body
        FROM agent_definitions a
        JOIN prompt_revisions er ON er.id = a.entry_prompt_revision_id
        JOIN prompt_revisions rr ON rr.id = a.review_prompt_revision_id
        ORDER BY a.id
    LOOP
        merged := agent.entry_body || CASE WHEN agent.kind = 'scheduled_prompt' THEN E'\n' ELSE E'\n\n' END || agent.review_body;
        candidate := agent.name;
        IF EXISTS (SELECT 1 FROM prompt_templates WHERE name = candidate AND kind = 'entry') THEN
            candidate := left(agent.name, 57) || '-prompt';
        END IF;
        IF EXISTS (SELECT 1 FROM prompt_templates WHERE name = candidate AND kind = 'entry') THEN
            candidate := left(agent.name, 44) || '-prompt-' || agent.id;
        END IF;
        INSERT INTO prompt_templates (name, kind) VALUES (candidate, 'entry') RETURNING id INTO new_template;
        INSERT INTO prompt_revisions (template_id, revision, body, content_sha256)
        VALUES (new_template, 1, merged, encode(sha256(convert_to(merged, 'UTF8')), 'hex'))
        RETURNING id INTO new_revision;
        UPDATE agent_definitions
        SET entry_prompt_revision_id = new_revision,
            review_prompt_revision_id = NULL,
            updated_at = now()
        WHERE id = agent.id;
    END LOOP;
END $$;

ALTER TABLE agent_definitions DROP COLUMN IF EXISTS review_prompt_revision_id;

DO $$
DECLARE
    review record;
    candidate text;
BEGIN
    FOR review IN SELECT id, name FROM prompt_templates WHERE kind = 'review' ORDER BY id LOOP
        candidate := review.name;
        IF EXISTS (SELECT 1 FROM prompt_templates WHERE name = candidate AND kind = 'entry') THEN
            candidate := left(review.name, 58) || '-focus';
        END IF;
        IF EXISTS (SELECT 1 FROM prompt_templates WHERE name = candidate AND kind = 'entry') THEN
            candidate := left(review.name, 45) || '-focus-' || review.id;
        END IF;
        UPDATE prompt_templates SET name = candidate, kind = 'entry' WHERE id = review.id;
    END LOOP;
END $$;

ALTER TABLE prompt_templates DROP CONSTRAINT IF EXISTS prompt_templates_kind_check;
ALTER TABLE prompt_templates ADD CONSTRAINT prompt_templates_kind_check CHECK (kind IN ('entry', 'plan', 'verify'));
