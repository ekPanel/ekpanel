-- Two-factor recovery codes: ten one-time codes per account, kept as SHA-256 hashes.
CREATE TABLE recovery_codes (
    user_id   uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash bytea       NOT NULL,
    used_at   timestamptz,
    PRIMARY KEY (user_id, code_hash)
);

-- Sessions get an id the panel can show and end, and say how they signed in.
ALTER TABLE sessions ADD COLUMN id uuid NOT NULL DEFAULT gen_random_uuid();
CREATE UNIQUE INDEX sessions_id_idx ON sessions (id);
ALTER TABLE sessions ADD COLUMN method text NOT NULL DEFAULT 'password';

-- Passkeys (WebAuthn). The browser knows an account by a random handle, not its id.
ALTER TABLE users ADD COLUMN webauthn_handle bytea UNIQUE;
CREATE TABLE passkeys (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_id bytea       NOT NULL UNIQUE,
    credential    jsonb       NOT NULL, -- go-webauthn's Credential
    name          text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz
);
CREATE INDEX passkeys_user_id_idx ON passkeys (user_id);
