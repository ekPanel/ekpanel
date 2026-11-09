-- API tokens: a user's keys for scripts and the ekctl CLI. Only the SHA-256 of a token is kept.
CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    token_hash   bytea       NOT NULL UNIQUE,
    -- The first characters of the token, so people can tell their tokens apart.
    prefix       text        NOT NULL,
    scopes       text[]      NOT NULL,
    expires_at   timestamptz,
    last_used_at timestamptz,
    last_used_ip text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_tokens_user ON api_tokens (user_id);
