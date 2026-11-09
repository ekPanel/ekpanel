-- The application installed on a site by ekPanel, if any (for now only 'wordpress').
ALTER TABLE sites ADD COLUMN app text CHECK (app IN ('wordpress'));
ALTER TABLE sites ADD COLUMN app_version text;
