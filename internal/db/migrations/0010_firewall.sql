-- Basic firewall per server, applied with ufw by the agent. 'unmanaged' (the default) leaves
-- whatever firewall the server already has alone; 'on' and 'off' are ekPanel's to keep.
ALTER TABLE servers ADD COLUMN firewall text NOT NULL DEFAULT 'unmanaged'
    CHECK (firewall IN ('unmanaged', 'on', 'off'));
-- The agent's last report: {"active":..,"ssh_ports":[..],"error":"..","at":".."}.
ALTER TABLE servers ADD COLUMN firewall_status jsonb;

CREATE TABLE firewall_rules (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id  uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    action     text        NOT NULL CHECK (action IN ('allow', 'deny')),
    protocol   text        NOT NULL CHECK (protocol IN ('tcp', 'udp', 'both')),
    port_from  integer CHECK (port_from BETWEEN 1 AND 65535),
    port_to    integer CHECK (port_to BETWEEN 1 AND 65535 AND port_to >= port_from),
    source     text        NOT NULL DEFAULT '', -- an address or CIDR; empty means anywhere
    note       text        NOT NULL DEFAULT '',
    created_by uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Opening a port needs a port; blocking needs an address.
    CHECK (action = 'deny' OR port_from IS NOT NULL),
    CHECK (action = 'allow' OR source <> '')
);
CREATE INDEX firewall_rules_server_idx ON firewall_rules (server_id, created_at);
