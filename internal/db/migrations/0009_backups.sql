-- Backup repositories (restic). The encryption password and any storage credentials live
-- only on the server's agent (shown to the user once as a recovery key); the panel keeps the
-- schedule, retention and a description of where backups go.
CREATE TABLE backup_repos (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id    uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    kind         text        NOT NULL CHECK (kind IN ('local', 's3')),
    location     text        NOT NULL,
    schedule     text        NOT NULL DEFAULT 'daily' CHECK (schedule IN ('off', 'hourly', 'daily', 'weekly')),
    hour_utc     integer     NOT NULL DEFAULT 2 CHECK (hour_utc BETWEEN 0 AND 23),
    keep_daily   integer     NOT NULL DEFAULT 7 CHECK (keep_daily BETWEEN 0 AND 365),
    keep_weekly  integer     NOT NULL DEFAULT 4 CHECK (keep_weekly BETWEEN 0 AND 520),
    keep_monthly integer     NOT NULL DEFAULT 6 CHECK (keep_monthly BETWEEN 0 AND 240),
    next_run_at  timestamptz,
    last_run_at  timestamptz,
    last_status  text CHECK (last_status IN ('succeeded', 'partial', 'failed')),
    last_message text,
    created_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX backup_repos_due_idx ON backup_repos (next_run_at) WHERE schedule <> 'off';
