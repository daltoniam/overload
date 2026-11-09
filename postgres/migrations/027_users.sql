-- People who sign in with OIDC. issuer and subject identify them; email and
-- name are for display. role is refreshed from the provider's groups at
-- every sign-in; disabled users cannot sign in and lose their sessions.
CREATE TABLE IF NOT EXISTS users (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    issuer text NOT NULL,
    subject text NOT NULL,
    email text NOT NULL DEFAULT '',
    name text NOT NULL DEFAULT '',
    role text NOT NULL,
    disabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    UNIQUE (issuer, subject)
);
-- Dashboard sessions. Only a SHA-256 of the cookie value is stored, with
-- the provider's ID token as the hint for signing out of the provider.
CREATE TABLE IF NOT EXISTS user_sessions (
    token_hash bytea PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    id_token text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS user_sessions_user ON user_sessions (user_id);
-- Sign-ins in progress: the state sent to the provider (hashed), with the
-- nonce and PKCE verifier to check its answer against.
CREATE TABLE IF NOT EXISTS oidc_logins (
    state_hash bytea PRIMARY KEY,
    nonce text NOT NULL,
    verifier text NOT NULL,
    next text NOT NULL DEFAULT '/',
    expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS oidc_logins_expiry ON oidc_logins (expires_at);
