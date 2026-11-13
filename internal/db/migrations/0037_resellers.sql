-- Resellers sell hosting to their own customers, within limits an administrator gives them.
-- The limits are totals over all their customers (what they have, not what their packages
-- allow), plus the most one package may give and what packages may offer at all.
CREATE TABLE resellers (
    user_id          uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    max_customers    integer NOT NULL CHECK (max_customers >= 0),
    max_sites        integer NOT NULL CHECK (max_sites >= 0),
    max_databases    integer NOT NULL CHECK (max_databases >= 0),
    max_apps         integer NOT NULL CHECK (max_apps >= 0),
    max_cron_jobs    integer NOT NULL CHECK (max_cron_jobs >= 0),
    max_mail_domains integer NOT NULL CHECK (max_mail_domains >= 0),
    max_mailboxes    integer NOT NULL CHECK (max_mailboxes >= 0),
    disk_mb          integer CHECK (disk_mb > 0),                       -- NULL: no limit
    bandwidth_gb     integer CHECK (bandwidth_gb > 0),                  -- per calendar month (UTC)
    bandwidth_stop   boolean NOT NULL DEFAULT false,
    -- The most one of the reseller's packages may give (NULL: any).
    memory_mb        integer CHECK (memory_mb >= 64),
    cpu_percent      integer CHECK (cpu_percent BETWEEN 5 AND 102400),
    mailbox_quota_mb integer CHECK (mailbox_quota_mb > 0),
    -- What packages may offer; empty: everything.
    php_versions     text[]  NOT NULL DEFAULT '{}',
    app_images       text[]  NOT NULL DEFAULT '{}',
    server_ids       uuid[]  NOT NULL DEFAULT '{}',
    sftp             boolean NOT NULL DEFAULT true,
    ftp              boolean NOT NULL DEFAULT true,
    git_apps         boolean NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- A reseller's customers; a reseller with customers cannot be deleted.
ALTER TABLE users ADD COLUMN reseller_id uuid REFERENCES users (id) ON DELETE RESTRICT;
CREATE INDEX users_reseller_id ON users (reseller_id) WHERE reseller_id IS NOT NULL;
ALTER TABLE users ADD CONSTRAINT users_reseller_customer CHECK (reseller_id IS NULL OR role = 'customer');
-- Who suspended an account: a reseller cannot lift what an administrator did.
ALTER TABLE users ADD COLUMN suspended_by uuid REFERENCES users (id) ON DELETE SET NULL;

-- A reseller's packages; names are unique per reseller (and among the administrators').
ALTER TABLE packages ADD COLUMN reseller_id uuid REFERENCES users (id) ON DELETE CASCADE;
DROP INDEX packages_name_key;
CREATE UNIQUE INDEX packages_name_key ON packages (COALESCE(reseller_id, '00000000-0000-0000-0000-000000000000'::uuid), lower(name));
