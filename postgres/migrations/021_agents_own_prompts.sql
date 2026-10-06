-- Agents and workflows own their prompts. Each agent gets its own prompt
-- (named agent:<agent>) whose first revision is the text it uses now, and
-- each workflow's planner and verifier get their own prompts (named
-- workflow:<workflow>, kinds plan and verify) the same way, so behaviour does
-- not change. Agents that shared a prompt get identical copies. Older
-- prompts are left in place, unused; runs keep their exact text in their
-- pinned snapshots.
DO $$
DECLARE
    agent record;
    workflow record;
    owned bigint;
    revision_id bigint;
    new_routing jsonb;
    prompt_kind text;
    routing_key text;
BEGIN
    FOR agent IN
        SELECT a.id, a.name, r.body, r.content_sha256
        FROM agent_definitions a
        JOIN prompt_revisions r ON r.id = a.entry_prompt_revision_id
        ORDER BY a.id
    LOOP
        INSERT INTO prompt_templates (name, kind) VALUES ('agent:' || agent.name, 'entry')
        ON CONFLICT (name, kind) DO UPDATE SET name = EXCLUDED.name
        RETURNING id INTO owned;
        INSERT INTO prompt_revisions (template_id, revision, body, content_sha256)
        VALUES (owned, (SELECT COALESCE(max(revision), 0) + 1 FROM prompt_revisions WHERE template_id = owned), agent.body, agent.content_sha256)
        RETURNING id INTO revision_id;
        UPDATE agent_definitions SET entry_prompt_revision_id = revision_id WHERE id = agent.id;
    END LOOP;

    FOR workflow IN SELECT id, name, routing AS current FROM workflows ORDER BY id LOOP
        new_routing := workflow.current - 'planner_prompt' - 'verifier_prompt';
        FOREACH prompt_kind IN ARRAY ARRAY['plan', 'verify'] LOOP
            routing_key := CASE prompt_kind WHEN 'plan' THEN 'planner_prompt_revision_id' ELSE 'verifier_prompt_revision_id' END;
            IF workflow.current ? routing_key THEN
                INSERT INTO prompt_templates (name, kind) VALUES ('workflow:' || workflow.name, prompt_kind)
                ON CONFLICT (name, kind) DO UPDATE SET name = EXCLUDED.name
                RETURNING id INTO owned;
                INSERT INTO prompt_revisions (template_id, revision, body, content_sha256)
                SELECT owned, (SELECT COALESCE(max(revision), 0) + 1 FROM prompt_revisions WHERE template_id = owned), r.body, r.content_sha256
                FROM prompt_revisions r WHERE r.id = (workflow.current ->> routing_key)::bigint
                RETURNING id INTO revision_id;
                IF revision_id IS NULL THEN
                    new_routing := new_routing - routing_key;
                ELSE
                    new_routing := jsonb_set(new_routing, ARRAY[routing_key], to_jsonb(revision_id));
                END IF;
                revision_id := NULL;
            END IF;
        END LOOP;
        UPDATE workflows SET routing = new_routing WHERE id = workflow.id;
    END LOOP;
END $$;
