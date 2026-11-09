-- Websites. A row is the desired state of one site; the server's agent converges to it.
-- `generation` increases with every change; `applied_generation` is the last one the
-- agent confirmed, so a site whose numbers differ is still being applied.
CREATE TABLE sites (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id          uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    domain             text        NOT NULL,
    aliases            text[]      NOT NULL DEFAULT '{}',
    kind               text        NOT NULL CHECK (kind IN ('static', 'php')),
    php_version        text,
    tls                text        NOT NULL DEFAULT 'auto' CHECK (tls IN ('auto', 'internal')),
    -- Linux account the site's files and PHP processes belong to.
    linux_user        text        NOT NULL UNIQUE,
    state              text        NOT NULL DEFAULT 'pending'
                       CHECK (state IN ('pending', 'active', 'error', 'deleting')),
    message            text,
    generation         bigint      NOT NULL DEFAULT 1,
    applied_generation bigint      NOT NULL DEFAULT 0,
    created_by         uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'php') = (php_version IS NOT NULL))
);
CREATE INDEX sites_server_idx ON sites (server_id);

-- Every host name a site answers to (its domain and aliases), unique across the panel.
CREATE TABLE site_hostnames (
    hostname text PRIMARY KEY,
    site_id  uuid NOT NULL REFERENCES sites (id) ON DELETE CASCADE
);

-- At most one queued task per key, so repeated changes to a server's sites collapse into
-- a single reconcile run.
ALTER TABLE tasks ADD COLUMN dedupe_key text;
CREATE UNIQUE INDEX tasks_dedupe_queued_idx ON tasks (dedupe_key) WHERE status = 'queued' AND dedupe_key IS NOT NULL;
