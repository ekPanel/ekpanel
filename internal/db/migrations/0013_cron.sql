-- Scheduled commands. A job runs as its website's Linux account, or inside one of its app's
-- containers, from a systemd timer on the server.
ALTER TABLE packages ADD COLUMN max_cron_jobs integer NOT NULL DEFAULT 5 CHECK (max_cron_jobs >= 0);

CREATE TABLE cron_jobs (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id          uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    site_id            uuid REFERENCES sites (id) ON DELETE CASCADE,
    app_id             uuid REFERENCES apps (id) ON DELETE CASCADE,
    service            text,                        -- apps: the Compose service to run in
    name               text        NOT NULL,
    schedule           text        NOT NULL,        -- five-field cron expression
    timezone           text        NOT NULL DEFAULT 'UTC',
    command            text        NOT NULL,
    timeout_seconds    integer     NOT NULL DEFAULT 3600 CHECK (timeout_seconds BETWEEN 10 AND 86400),
    enabled            boolean     NOT NULL DEFAULT true,
    state              text        NOT NULL DEFAULT 'pending'
                       CHECK (state IN ('pending', 'active', 'error', 'deleting')),
    message            text,
    generation         bigint      NOT NULL DEFAULT 1,
    applied_generation bigint      NOT NULL DEFAULT 0,
    created_by         uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK ((site_id IS NULL) <> (app_id IS NULL)),
    CHECK ((app_id IS NULL) = (service IS NULL))
);
CREATE INDEX cron_jobs_server_idx ON cron_jobs (server_id);
CREATE INDEX cron_jobs_site_idx ON cron_jobs (site_id) WHERE site_id IS NOT NULL;
CREATE INDEX cron_jobs_app_idx ON cron_jobs (app_id) WHERE app_id IS NOT NULL;
