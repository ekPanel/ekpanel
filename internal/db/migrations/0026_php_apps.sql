-- Applications ekPanel installs besides WordPress.
ALTER TABLE sites DROP CONSTRAINT IF EXISTS sites_app_check;
ALTER TABLE sites ADD CONSTRAINT sites_app_check CHECK (app IN ('wordpress', 'joomla', 'drupal', 'moodle', 'laravel'));
