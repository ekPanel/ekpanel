-- Background tasks: work the panel does (often by asking an agent), with its progress
-- visible to users. Workers claim queued rows with FOR UPDATE SKIP LOCKED; a row whose
-- lease expires while running belonged to a crashed worker and is retried or failed.
CREATE TABLE tasks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,
    server_id    uuid        REFERENCES servers (id) ON DELETE CASCADE,
    params       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    status       text        NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    attempts     integer     NOT NULL DEFAULT 0,
    max_attempts integer     NOT NULL DEFAULT 1,
    result       jsonb,
    error        text,
    created_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    run_after    timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz,
    locked_until timestamptz
);
CREATE INDEX tasks_queued_idx ON tasks (run_after) WHERE status = 'queued';
CREATE INDEX tasks_running_idx ON tasks (locked_until) WHERE status = 'running';
CREATE INDEX tasks_server_idx ON tasks (server_id, created_at DESC);
