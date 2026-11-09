-- A website can also be rebuilt on another server from a backup (when its server is gone):
-- the move then names the backup repository (on the new server) it was restored from.
ALTER TABLE moves ADD COLUMN from_backup uuid REFERENCES backup_repos (id) ON DELETE SET NULL;
