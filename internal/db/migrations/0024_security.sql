-- CrowdSec per server: whether it runs, the addresses it never bans (besides the server's
-- own and Cloudflare's), and what the last apply reported.
ALTER TABLE servers ADD COLUMN crowdsec boolean NOT NULL DEFAULT false;
ALTER TABLE servers ADD COLUMN allowlist text[] NOT NULL DEFAULT '{}';
ALTER TABLE servers ADD COLUMN security_status jsonb;
ALTER TABLE servers ADD COLUMN security_checked_at timestamptz;
