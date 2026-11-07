-- Redirects, hotlink protection and PHP settings of a site (agentproto.SiteWeb).
ALTER TABLE sites ADD COLUMN web jsonb NOT NULL DEFAULT '{}';

-- A certificate the user uploads instead of a free one. The key is sealed with the panel's
-- secret key; the expiry and names are kept to show and to warn before it expires.
ALTER TABLE sites DROP CONSTRAINT sites_tls_check;
ALTER TABLE sites ADD CONSTRAINT sites_tls_check CHECK (tls IN ('auto', 'internal', 'custom'));
ALTER TABLE sites ADD COLUMN tls_cert text;
ALTER TABLE sites ADD COLUMN tls_key bytea;
ALTER TABLE sites ADD COLUMN tls_expires_at timestamptz;
ALTER TABLE sites ADD COLUMN tls_names text[];
ALTER TABLE sites ADD CONSTRAINT sites_custom_tls_check CHECK ((tls = 'custom') = (tls_cert IS NOT NULL AND tls_key IS NOT NULL));
