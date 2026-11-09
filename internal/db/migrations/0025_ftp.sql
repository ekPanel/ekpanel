-- FTP per site, over TLS only: a login of its own (the site's account name) with only its
-- SHA-512 crypt hash kept. Packages say whether customers may turn it on.
ALTER TABLE sites ADD COLUMN ftp_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE sites ADD COLUMN ftp_password_hash text;
ALTER TABLE packages ADD COLUMN ftp boolean NOT NULL DEFAULT false;
