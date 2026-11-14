-- A database can move on its own: a move of neither a site nor an app, with one database.
ALTER TABLE moves DROP CONSTRAINT moves_one_thing;
ALTER TABLE moves ADD CONSTRAINT moves_one_thing CHECK (
    num_nonnulls(site_id, app_id) = 1 OR (site_id IS NULL AND app_id IS NULL AND cardinality(database_ids) = 1)
) NOT VALID;
