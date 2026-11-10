-- Eval suites saved by people: cases built from commits that fixed real
-- bugs, with the lines each fix changed.
CREATE TABLE IF NOT EXISTS bakeoff_suites (
    name text PRIMARY KEY,
    description text NOT NULL DEFAULT '',
    cases jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- A bake-off runs suites against models; models holds the pinned model
-- settings and suites the pinned cases, so editing either later does not
-- change a bake-off.
CREATE TABLE IF NOT EXISTS bakeoffs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status text NOT NULL DEFAULT 'queued',
    models jsonb NOT NULL,
    suites jsonb NOT NULL,
    runs integer NOT NULL,
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz
);
CREATE TABLE IF NOT EXISTS bakeoff_results (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    bakeoff_id bigint NOT NULL REFERENCES bakeoffs(id) ON DELETE CASCADE,
    model text NOT NULL,
    suite text NOT NULL,
    case_name text NOT NULL,
    run integer NOT NULL,
    expected integer NOT NULL DEFAULT 0,
    matched integer NOT NULL DEFAULT 0,
    misplaced integer NOT NULL DEFAULT 0,
    reported integer NOT NULL DEFAULT 0,
    -- Findings that matched no expected bug, not counting misplaced ones.
    unmatched integer NOT NULL DEFAULT 0,
    -- Expected bugs with words a misplaced finding could be recognised by
    -- (none for cases built from commits).
    placement_checked integer NOT NULL DEFAULT 0,
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    duration_ms bigint NOT NULL DEFAULT 0,
    error text NOT NULL DEFAULT '',
    UNIQUE (bakeoff_id, model, suite, case_name, run)
);
