-- Self-update: the server that runs the panel is the one that can update it.
ALTER TABLE servers ADD COLUMN panel_host boolean NOT NULL DEFAULT false;
