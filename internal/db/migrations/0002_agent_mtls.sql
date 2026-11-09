-- Agents authenticate with client certificates from the panel's CA instead of a bearer
-- secret. The panel keeps the fingerprint of each agent's current certificate, so
-- re-enrolling or deleting a server revokes the old certificate immediately.
ALTER TABLE servers DROP COLUMN agent_secret_hash;
ALTER TABLE servers ADD COLUMN agent_cert_sha256 text;
ALTER TABLE servers ADD COLUMN agent_cert_expires_at timestamptz;
