-- A server's addresses as its agent reports them, and the public addresses DNS records
-- point at: found among those, or set by hand (for servers behind NAT).
ALTER TABLE servers ADD COLUMN addresses text[] NOT NULL DEFAULT '{}';
ALTER TABLE servers ADD COLUMN public_ipv4 inet;
ALTER TABLE servers ADD COLUMN public_ipv6 inet;

-- Cloudflare accounts, reached with an API token sealed with the panel's key. Owned by a
-- customer, or by the administrators (owner_id NULL).
CREATE TABLE cloudflare_accounts (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id     uuid REFERENCES users (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    token        bytea       NOT NULL,
    account_id   text,       -- Cloudflare's, when the token may list accounts (to add zones)
    account_name text,
    created_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Zones ekPanel keeps in sync: each website's and app's host names under the zone get A and
-- AAAA records for its server. A customer's zone only gets their own.
CREATE TABLE dns_zones (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    uuid        NOT NULL REFERENCES cloudflare_accounts (id) ON DELETE CASCADE,
    owner_id      uuid REFERENCES users (id) ON DELETE CASCADE,
    cf_zone_id    text        NOT NULL,
    name          text        NOT NULL UNIQUE,
    status        text        NOT NULL DEFAULT 'active',
    name_servers  text[]      NOT NULL DEFAULT '{}',
    proxied       boolean     NOT NULL DEFAULT true, -- for new records
    -- What the last sync could not do by itself: records changed by hand, conflicts, servers
    -- without a public address (a JSON list), and why the sync failed, if it did.
    issues        jsonb       NOT NULL DEFAULT '[]',
    sync_error    text,
    synced_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- The records ekPanel made, as it last wrote them, so it changes or removes only those, and
-- notices when someone changed one by hand.
CREATE TABLE dns_records (
    zone_id      uuid    NOT NULL REFERENCES dns_zones (id) ON DELETE CASCADE,
    cf_record_id text    NOT NULL,
    type         text    NOT NULL,
    name         text    NOT NULL,
    content      text    NOT NULL,
    proxied      boolean NOT NULL,
    PRIMARY KEY (zone_id, cf_record_id),
    UNIQUE (zone_id, type, name)
);
