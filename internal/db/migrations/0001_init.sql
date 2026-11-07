-- Users, sessions, managed servers and the append-only audit log.

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL,
    name          text NOT NULL,
    password_hash text NOT NULL,
    role          text NOT NULL CHECK (role IN ('admin', 'reseller', 'customer')),
    locale        text NOT NULL DEFAULT 'en',
    totp_secret   text,
    totp_enabled  boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

-- Only the SHA-256 of a session token is stored, so a database leak does not leak sessions.
CREATE TABLE sessions (
    token_hash   bytea PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    ip           text NOT NULL,
    user_agent   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

CREATE TABLE servers (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name              text NOT NULL,
    -- One-time enrolment token (hash) handed to the agent installer; cleared after use.
    enroll_token_hash bytea,
    enroll_expires_at timestamptz,
    -- Long-lived agent credential (hash), issued at enrolment.
    agent_secret_hash bytea,
    enrolled_at       timestamptz,
    hostname          text NOT NULL DEFAULT '',
    os                text NOT NULL DEFAULT '',
    kernel            text NOT NULL DEFAULT '',
    arch              text NOT NULL DEFAULT '',
    cpu_count         integer NOT NULL DEFAULT 0,
    agent_version     text NOT NULL DEFAULT '',
    last_seen_at      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    actor_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_ip   text NOT NULL DEFAULT '',
    action     text NOT NULL,
    target     text NOT NULL DEFAULT '',
    details    jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);

-- The audit log is append-only: nobody, including the panel itself, may edit or delete rows.
-- (ON DELETE SET NULL on actor_id still works because it is an UPDATE of only that column.)
CREATE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.actor_id IS NULL
       AND (NEW.id, NEW.at, NEW.actor_ip, NEW.action, NEW.target, NEW.details)
           IS NOT DISTINCT FROM (OLD.id, OLD.at, OLD.actor_ip, OLD.action, OLD.target, OLD.details) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
