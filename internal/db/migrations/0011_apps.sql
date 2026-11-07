-- Docker apps. Every app is a Docker Compose project on its server: either one image with
-- settings from the panel, or a Compose file written by the user. A row is the desired
-- state; the agent converges to it like it does for sites.
CREATE TABLE apps (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id          uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name               text        NOT NULL,
    kind               text        NOT NULL CHECK (kind IN ('image', 'compose')),
    image              text,
    compose            text,
    -- The service and port inside it that the domain points at. The agent publishes that
    -- port on 127.0.0.1:host_port only, so it never bypasses the firewall.
    service            text,
    container_port     integer CHECK (container_port BETWEEN 1 AND 65535),
    host_port          integer CHECK (host_port BETWEEN 20000 AND 29999),
    domain             text,
    tls                text        NOT NULL DEFAULT 'auto' CHECK (tls IN ('auto', 'internal')),
    env                jsonb       NOT NULL DEFAULT '[]',
    volumes            text[]      NOT NULL DEFAULT '{}', -- container paths kept across updates (image apps)
    memory_mb          integer CHECK (memory_mb BETWEEN 16 AND 1048576),
    cpus               numeric(6, 2) CHECK (cpus > 0 AND cpus <= 1024),
    public_ports       boolean     NOT NULL DEFAULT false,
    stopped            boolean     NOT NULL DEFAULT false,
    delete_data        boolean     NOT NULL DEFAULT false,
    state              text        NOT NULL DEFAULT 'pending'
                       CHECK (state IN ('pending', 'active', 'error', 'deleting')),
    message            text,
    generation         bigint      NOT NULL DEFAULT 1,
    applied_generation bigint      NOT NULL DEFAULT 0,
    created_by         uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (server_id, name),
    UNIQUE (server_id, host_port),
    CHECK ((kind = 'image') = (image IS NOT NULL)),
    CHECK ((kind = 'compose') = (compose IS NOT NULL)),
    CHECK ((container_port IS NULL) = (host_port IS NULL)),
    CHECK (domain IS NULL OR container_port IS NOT NULL)
);
CREATE INDEX apps_server_idx ON apps (server_id);

-- Host names stay unique across websites and apps.
ALTER TABLE site_hostnames ALTER COLUMN site_id DROP NOT NULL;
ALTER TABLE site_hostnames ADD COLUMN app_id uuid REFERENCES apps (id) ON DELETE CASCADE;
ALTER TABLE site_hostnames ADD CHECK ((site_id IS NULL) <> (app_id IS NULL));
