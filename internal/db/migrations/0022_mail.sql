-- Email. A server with a mail row runs a mail server (Stalwart) and webmail (Roundcube) at
-- its mail host name. Turning email off keeps the mail data on the server.
CREATE TABLE mail_servers (
    server_id  uuid PRIMARY KEY REFERENCES servers (id) ON DELETE CASCADE,
    hostname   text        NOT NULL UNIQUE,
    tls        text        NOT NULL DEFAULT 'auto' CHECK (tls IN ('auto', 'internal')),
    -- Result of the last apply of the mail server itself (domains report their own).
    state      text        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'active', 'error')),
    message    text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Mail domains are desired state like sites: a change to the domain, its mailboxes or its
-- forwarders raises its generation, and the agent reports the generation it applied.
CREATE TABLE mail_domains (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id          uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE, -- with email on (mail_servers)
    owner_id           uuid        REFERENCES users (id) ON DELETE RESTRICT,
    domain             text        NOT NULL UNIQUE,
    catch_all          text,       -- local part of the mailbox that gets mail for unknown addresses
    state              text        NOT NULL DEFAULT 'pending'
                       CHECK (state IN ('pending', 'active', 'error', 'deleting')),
    message            text,
    -- The records the domain needs (MX, SPF, DKIM, DMARC, TLS reporting), as the mail
    -- server last reported them: published to Cloudflare when the domain's zone is there.
    dns_records        jsonb       NOT NULL DEFAULT '[]',
    generation         bigint      NOT NULL DEFAULT 1,
    applied_generation bigint      NOT NULL DEFAULT 0,
    created_by         uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mail_domains_server_idx ON mail_domains (server_id);
CREATE INDEX mail_domains_owner_idx ON mail_domains (owner_id) WHERE owner_id IS NOT NULL;

-- Only the SHA-512 crypt hash of a mailbox password is kept.
CREATE TABLE mailboxes (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    domain_id     uuid        NOT NULL REFERENCES mail_domains (id) ON DELETE CASCADE,
    local_part    text        NOT NULL,
    password_hash text        NOT NULL,
    quota_mb      integer     NOT NULL DEFAULT 0 CHECK (quota_mb >= 0), -- 0: no limit
    forward_to    text[]      NOT NULL DEFAULT '{}',
    keep_copy     boolean     NOT NULL DEFAULT true,
    auto_reply    jsonb,      -- {subject, body, start, end}; NULL: off
    used_bytes    bigint,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (domain_id, local_part)
);

CREATE TABLE mail_forwarders (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    domain_id    uuid        NOT NULL REFERENCES mail_domains (id) ON DELETE CASCADE,
    local_part   text        NOT NULL,
    destinations text[]      NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (domain_id, local_part)
);

-- What a package allows: mail domains and mailboxes (counted over all the customer's
-- domains), and the largest quota one mailbox may have (NULL: no limit).
ALTER TABLE packages ADD COLUMN max_mail_domains integer NOT NULL DEFAULT 0 CHECK (max_mail_domains >= 0);
ALTER TABLE packages ADD COLUMN max_mailboxes integer NOT NULL DEFAULT 0 CHECK (max_mailboxes >= 0);
ALTER TABLE packages ADD COLUMN mailbox_quota_mb integer CHECK (mailbox_quota_mb > 0);
