-- Service apps (4h-1): a command that systemd runs from a folder of one of the owner's
-- websites, as that website's account, instead of a container. Node, Go and other binaries,
-- and workers such as queue runners. The code stays in the website, so its files, SFTP and
-- backups cover it; the app has its own domain and port.
ALTER TABLE apps DROP CONSTRAINT apps_kind_check;
ALTER TABLE apps ADD CONSTRAINT apps_kind_check CHECK (kind IN ('image', 'compose', 'git', 'service'));

ALTER TABLE apps ADD COLUMN site_id         uuid REFERENCES sites (id) ON DELETE RESTRICT;
ALTER TABLE apps ADD COLUMN directory       text; -- inside the website's home folder; '' is the home itself
ALTER TABLE apps ADD COLUMN command         text;
ALTER TABLE apps ADD COLUMN build_command   text;
ALTER TABLE apps ADD COLUMN runtime         text NOT NULL DEFAULT '' CHECK (runtime IN ('', 'node'));
ALTER TABLE apps ADD COLUMN runtime_version text;
ALTER TABLE apps ADD COLUMN build_state     text NOT NULL DEFAULT ''
                                            CHECK (build_state IN ('', 'running', 'ok', 'failed'));
ALTER TABLE apps ADD COLUMN build_log       text;
ALTER TABLE apps ADD COLUMN built_at        timestamptz;

ALTER TABLE apps ADD CONSTRAINT apps_service_check
    CHECK ((kind = 'service') = (site_id IS NOT NULL AND directory IS NOT NULL AND command IS NOT NULL));
ALTER TABLE apps ADD CONSTRAINT apps_build_command_check CHECK (kind = 'service' OR build_command IS NULL);
ALTER TABLE apps ADD CONSTRAINT apps_runtime_version_check CHECK ((runtime = '') = (runtime_version IS NULL));

-- A service listens on its own port, which may be one the program insists on (above 1023).
-- The port its domain points at is the same, so container_port equals host_port.
ALTER TABLE apps DROP CONSTRAINT apps_host_port_check;
ALTER TABLE apps ADD CONSTRAINT apps_host_port_check
    CHECK (host_port BETWEEN 20000 AND 29999 OR (kind = 'service' AND host_port BETWEEN 1024 AND 65535));
ALTER TABLE apps ADD CONSTRAINT apps_service_port_check CHECK (kind <> 'service' OR container_port IS NOT DISTINCT FROM host_port);

CREATE INDEX apps_site_idx ON apps (site_id) WHERE site_id IS NOT NULL;
