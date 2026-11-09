-- Server metrics over time. One row per server per bucket: 60-second buckets for the last
-- two days, and 1-hour buckets (rolled up from them) for the last 31 days.
CREATE TABLE server_metrics (
    server_id   uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    resolution  integer     NOT NULL CHECK (resolution IN (60, 3600)),
    bucket      timestamptz NOT NULL,
    samples     integer     NOT NULL,
    cpu_percent real        NOT NULL,
    load1       real        NOT NULL,
    mem_used    bigint      NOT NULL,
    mem_total   bigint      NOT NULL,
    disk_used   bigint      NOT NULL,
    disk_total  bigint      NOT NULL,
    net_rx_rate real        NOT NULL,
    net_tx_rate real        NOT NULL,
    PRIMARY KEY (server_id, resolution, bucket)
);
