-- WordPress toolkit. What the last scan found on a site (versions, updates, plugins, themes,
-- settings), and the updates ekPanel makes by itself: core "off", "minor" or "all", and
-- whether plugins and themes are updated too.
ALTER TABLE sites ADD COLUMN wp_info jsonb;
ALTER TABLE sites ADD COLUMN wp_scanned_at timestamptz;
ALTER TABLE sites ADD COLUMN wp_auto jsonb NOT NULL DEFAULT '{"core": "minor", "plugins": false, "themes": false}';

-- A staging copy is a site of its own that points at the live site it copies. A site has
-- at most one; deleting the live site leaves the copy as an ordinary site.
ALTER TABLE sites ADD COLUMN staging_of uuid REFERENCES sites (id) ON DELETE SET NULL;
CREATE UNIQUE INDEX sites_staging_idx ON sites (staging_of) WHERE staging_of IS NOT NULL;
