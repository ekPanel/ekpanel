-- Customer databases (MariaDB or PostgreSQL) with one owner account each. Like sites, a
-- row is desired state and the agent converges to it. Only the engine's own password hash
-- is stored (MariaDB native hash, PostgreSQL SCRAM-SHA-256 verifier), never the password.
CREATE TABLE databases (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id          uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    engine             text        NOT NULL CHECK (engine IN ('mariadb', 'postgresql')),
    name               text        NOT NULL,
    username           text        NOT NULL,
    password_hash      text        NOT NULL,
    state              text        NOT NULL DEFAULT 'pending'
                       CHECK (state IN ('pending', 'active', 'error', 'deleting')),
    message            text,
    size_bytes         bigint,
    generation         bigint      NOT NULL DEFAULT 1,
    applied_generation bigint      NOT NULL DEFAULT 0,
    created_by         uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (server_id, engine, name),
    UNIQUE (server_id, engine, username)
);
CREATE INDEX databases_server_idx ON databases (server_id);
