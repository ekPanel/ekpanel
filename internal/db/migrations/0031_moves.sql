-- Moving a website (and the databases chosen with it) to another server. The source keeps
-- serving the old copy until someone removes it (phase "cleaning", then "done").
CREATE TABLE moves (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id      uuid        REFERENCES sites (id) ON DELETE CASCADE,
    from_server  uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    to_server    uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    database_ids uuid[]      NOT NULL DEFAULT '{}',
    -- What the old copy is on the source, so it can be removed later.
    linux_user   text        NOT NULL DEFAULT '',
    databases    jsonb       NOT NULL DEFAULT '[]',
    phase        text        NOT NULL DEFAULT 'copying'
                 CHECK (phase IN ('copying', 'switching', 'moved', 'cleaning', 'done', 'failed')),
    error        text,
    bytes        bigint      NOT NULL DEFAULT 0,
    -- What the site's home page answered on the new server.
    check_status integer,
    -- After a failed switch, the target's half-made copy is removed by its next apply.
    target_cleanup boolean   NOT NULL DEFAULT false,
    created_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    moved_at     timestamptz,
    finished_at  timestamptz
);
CREATE INDEX moves_site ON moves (site_id);
CREATE INDEX moves_from ON moves (from_server) WHERE phase IN ('switching', 'moved', 'cleaning');
CREATE INDEX moves_to ON moves (to_server) WHERE target_cleanup;
-- One move of a site at a time, until its old copy is removed or it failed.
CREATE UNIQUE INDEX moves_one_open ON moves (site_id) WHERE phase IN ('copying', 'switching', 'moved', 'cleaning');
