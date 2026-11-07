-- Git apps: built on their server from a repository's Dockerfile and released without
-- downtime. Their image is the build that is live (NULL before the first deploy). Each gets
-- two consecutive host ports, host_port and host_port + 1: a new release starts on the free
-- one while the old keeps serving, and the web server uses whichever answers.
ALTER TABLE apps DROP CONSTRAINT apps_kind_check;
ALTER TABLE apps ADD CONSTRAINT apps_kind_check CHECK (kind IN ('image', 'compose', 'git'));
-- The unnamed check that an image app has an image, and only image apps do.
DO $$
DECLARE c text;
BEGIN
    SELECT conname INTO STRICT c FROM pg_constraint
    WHERE conrelid = 'apps'::regclass AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%(kind = ''image''::text) = (image IS NOT NULL)%';
    EXECUTE format('ALTER TABLE apps DROP CONSTRAINT %I', c);
END $$;
ALTER TABLE apps ADD CONSTRAINT apps_image_check CHECK (kind <> 'image' OR image IS NOT NULL);
ALTER TABLE apps ADD CONSTRAINT apps_image_kind_check CHECK (kind IN ('image', 'git') OR image IS NULL);

ALTER TABLE apps ADD COLUMN git_url text;
ALTER TABLE apps ADD COLUMN git_branch text;
ALTER TABLE apps ADD COLUMN dockerfile text;    -- path in the repository
ALTER TABLE apps ADD COLUMN build_context text; -- folder in the repository
ALTER TABLE apps ADD COLUMN auto_deploy boolean NOT NULL DEFAULT true;
-- Sealed with the panel's secret key: the webhook's signing secret and the deploy key.
ALTER TABLE apps ADD COLUMN webhook_secret bytea;
ALTER TABLE apps ADD COLUMN deploy_key bytea;
ALTER TABLE apps ADD COLUMN deploy_key_public text;
ALTER TABLE apps ADD CONSTRAINT apps_git_check CHECK ((kind = 'git') = (git_url IS NOT NULL AND git_branch IS NOT NULL));
ALTER TABLE apps ADD CONSTRAINT apps_git_port_check CHECK (kind <> 'git' OR container_port IS NOT NULL);

CREATE TABLE deployments (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id         uuid        NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    number         integer     NOT NULL,
    status         text        NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued', 'building', 'releasing', 'live', 'failed', 'superseded', 'cancelled')),
    trigger        text        NOT NULL CHECK (trigger IN ('manual', 'webhook', 'rollback')),
    branch         text        NOT NULL,
    commit_sha     text,       -- wanted (from a webhook) until the build reports what it built
    commit_message text,
    image          text,       -- set once built, or the image a rollback returns to
    rollback_of    uuid        REFERENCES deployments (id) ON DELETE SET NULL,
    log            text        NOT NULL DEFAULT '',
    error          text,
    created_by     uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    started_at     timestamptz,
    finished_at    timestamptz,
    UNIQUE (app_id, number)
);
CREATE INDEX deployments_app_idx ON deployments (app_id, number DESC);
