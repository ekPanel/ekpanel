-- Hosting packages and customer accounts. A customer uses the panel like an administrator,
-- but sees and changes only what they own, within the limits of their package.
CREATE TABLE packages (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        NOT NULL,
    max_sites     integer     NOT NULL DEFAULT 5 CHECK (max_sites >= 0),
    max_databases integer     NOT NULL DEFAULT 5 CHECK (max_databases >= 0),
    max_apps      integer     NOT NULL DEFAULT 0 CHECK (max_apps >= 0), -- 0: no Docker apps
    disk_mb       integer CHECK (disk_mb > 0),                          -- NULL: no limit
    memory_mb     integer CHECK (memory_mb >= 64),                      -- for all the customer's processes
    cpu_percent   integer CHECK (cpu_percent BETWEEN 5 AND 102400),     -- 100 = one core
    php_versions  text[]      NOT NULL DEFAULT '{}',                    -- empty: every version
    app_images    text[]      NOT NULL DEFAULT '{}',                    -- allowed image prefixes; empty: any image
    server_ids    uuid[]      NOT NULL DEFAULT '{}',                    -- servers the customer may use
    sftp          boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX packages_name_key ON packages (lower(name));

ALTER TABLE users ADD COLUMN package_id uuid REFERENCES packages (id) ON DELETE RESTRICT;
ALTER TABLE users ADD CONSTRAINT users_customer_package CHECK (role <> 'customer' OR package_id IS NOT NULL);

-- Owner of each resource; NULL means it belongs to the administrators. A customer cannot be
-- deleted while they still own something.
ALTER TABLE sites ADD COLUMN owner_id uuid REFERENCES users (id) ON DELETE RESTRICT;
ALTER TABLE databases ADD COLUMN owner_id uuid REFERENCES users (id) ON DELETE RESTRICT;
ALTER TABLE apps ADD COLUMN owner_id uuid REFERENCES users (id) ON DELETE RESTRICT;
CREATE INDEX sites_owner_idx ON sites (owner_id) WHERE owner_id IS NOT NULL;
CREATE INDEX databases_owner_idx ON databases (owner_id) WHERE owner_id IS NOT NULL;
CREATE INDEX apps_owner_idx ON apps (owner_id) WHERE owner_id IS NOT NULL;

-- Disk use, measured by the agent; databases already report size_bytes.
ALTER TABLE sites ADD COLUMN disk_bytes bigint;
ALTER TABLE apps ADD COLUMN disk_bytes bigint;

-- An administrator signed in as a customer: the session belongs to the customer, the audit
-- log names the administrator.
ALTER TABLE sessions ADD COLUMN impersonator_id uuid REFERENCES users (id) ON DELETE CASCADE;
