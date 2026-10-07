-- The account that owns the GitHub App, so the UI can link to the App's
-- settings page (personal and organization Apps live at different URLs).
ALTER TABLE github_app ADD COLUMN IF NOT EXISTS owner_login text NOT NULL DEFAULT '';
ALTER TABLE github_app ADD COLUMN IF NOT EXISTS owner_type text NOT NULL DEFAULT '';
