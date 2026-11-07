-- SFTP access per website. The site's Linux account can sign in over SFTP, locked into its
-- own folder. Only a SHA-512 crypt hash of the password is kept; SSH keys are public keys.
ALTER TABLE sites ADD COLUMN sftp_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE sites ADD COLUMN sftp_password_hash text;
ALTER TABLE sites ADD COLUMN ssh_keys text[] NOT NULL DEFAULT '{}';
