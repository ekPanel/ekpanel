-- Built-in DNS. Name servers are ekPanel servers running PowerDNS: one primary, which holds
-- and signs the zones, and secondaries, which copy them by zone transfer. Every hosted zone
-- is served by all of them.
CREATE TABLE name_servers (
    server_id        uuid PRIMARY KEY REFERENCES servers (id) ON DELETE CASCADE,
    hostname         text        NOT NULL UNIQUE, -- ns1.example.com
    is_primary       boolean     NOT NULL DEFAULT false,
    -- The address the other name servers use for zone transfers and NOTIFY; NULL means
    -- the server's public address.
    transfer_address inet,
    position         int         NOT NULL DEFAULT 0,
    status           jsonb,       -- what the last apply reported
    checked_at       timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX name_servers_one_primary ON name_servers (is_primary) WHERE is_primary;

-- Zones on ekPanel's own name servers. Owned by a customer, or by the administrators.
CREATE TABLE hosted_zones (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   uuid REFERENCES users (id) ON DELETE CASCADE,
    name       text        NOT NULL UNIQUE,
    dnssec     boolean     NOT NULL DEFAULT false,
    -- From the primary: the zone's serial, the DS records for the registrar, and why the
    -- last apply failed, if it did.
    serial     bigint      NOT NULL DEFAULT 0,
    ds         text[]      NOT NULL DEFAULT '{}',
    error      text,
    -- Records ekPanel could not make because the zone's own records are in the way.
    issues     jsonb       NOT NULL DEFAULT '[]',
    created_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The records people keep in a zone. Records for websites, apps, email and the name
-- servers themselves are made when the zone is applied and not stored here.
CREATE TABLE hosted_records (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    zone_id    uuid    NOT NULL REFERENCES hosted_zones (id) ON DELETE CASCADE,
    name       text    NOT NULL,
    type       text    NOT NULL,
    content    text    NOT NULL,
    priority   int     NOT NULL DEFAULT 0,
    ttl        int     NOT NULL DEFAULT 3600,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (zone_id, name, type, content)
);
CREATE INDEX hosted_records_zone ON hosted_records (zone_id);

-- Records added to new zones, "@" standing for the zone's own name.
CREATE TABLE dns_templates (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL UNIQUE,
    records    jsonb       NOT NULL DEFAULT '[]',
    created_at timestamptz NOT NULL DEFAULT now()
);
