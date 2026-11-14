-- Apps move between servers too (see 0031_moves.sql).
ALTER TABLE moves ADD COLUMN app_id uuid REFERENCES apps (id) ON DELETE CASCADE;
ALTER TABLE moves ADD CONSTRAINT moves_one_thing CHECK ((site_id IS NULL) <> (app_id IS NULL)) NOT VALID;
CREATE INDEX moves_app ON moves (app_id);
CREATE UNIQUE INDEX moves_one_open_app ON moves (app_id) WHERE phase IN ('copying', 'switching', 'moved', 'cleaning');
