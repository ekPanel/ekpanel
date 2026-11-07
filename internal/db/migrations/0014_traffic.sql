-- Requests per website or app per hour, from the web server's log. Kept for 90 days.
CREATE TABLE traffic_hourly (
    site_id  uuid REFERENCES sites (id) ON DELETE CASCADE,
    app_id   uuid REFERENCES apps (id) ON DELETE CASCADE,
    hour     timestamptz NOT NULL,
    requests bigint      NOT NULL,
    bytes    bigint      NOT NULL,
    s2xx     bigint      NOT NULL,
    s3xx     bigint      NOT NULL,
    s4xx     bigint      NOT NULL,
    s5xx     bigint      NOT NULL,
    paths    jsonb       NOT NULL DEFAULT '{}', -- most requested paths in the hour
    CHECK ((site_id IS NULL) <> (app_id IS NULL))
);
CREATE UNIQUE INDEX traffic_site_hour ON traffic_hourly (site_id, hour) WHERE site_id IS NOT NULL;
CREATE UNIQUE INDEX traffic_app_hour ON traffic_hourly (app_id, hour) WHERE app_id IS NOT NULL;
CREATE INDEX traffic_hour_idx ON traffic_hourly (hour);
