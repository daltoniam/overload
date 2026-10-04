CREATE TABLE IF NOT EXISTS github_app (
    id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    app_id bigint NOT NULL CHECK (app_id > 0),
    slug text NOT NULL,
    html_url text NOT NULL DEFAULT '',
    private_key text NOT NULL,
    webhook_secret text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
