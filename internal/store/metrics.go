package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// MetricPoint is one bucket of server metrics. Rates are averages over the bucket; memory
// and disk are the last value seen in it.
type MetricPoint struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	Load1      float64   `json:"load1"`
	MemUsed    int64     `json:"mem_used"`
	MemTotal   int64     `json:"mem_total"`
	DiskUsed   int64     `json:"disk_used"`
	DiskTotal  int64     `json:"disk_total"`
	NetRxRate  float64   `json:"net_rx_rate"`
	NetTxRate  float64   `json:"net_tx_rate"`
}

const (
	MinuteRetention = 48 * time.Hour
	HourRetention   = 31 * 24 * time.Hour
)

// SaveMinute stores (or, for a reconnecting agent, merges into) a 60-second bucket.
func (s *Store) SaveMinute(ctx context.Context, serverID string, p MetricPoint, samples int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO server_metrics (server_id, resolution, bucket, samples, cpu_percent, load1,
			mem_used, mem_total, disk_used, disk_total, net_rx_rate, net_tx_rate)
		VALUES ($1, 60, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (server_id, resolution, bucket) DO UPDATE SET
			cpu_percent = (server_metrics.cpu_percent * server_metrics.samples + EXCLUDED.cpu_percent * EXCLUDED.samples)
			              / (server_metrics.samples + EXCLUDED.samples),
			load1       = (server_metrics.load1 * server_metrics.samples + EXCLUDED.load1 * EXCLUDED.samples)
			              / (server_metrics.samples + EXCLUDED.samples),
			net_rx_rate = (server_metrics.net_rx_rate * server_metrics.samples + EXCLUDED.net_rx_rate * EXCLUDED.samples)
			              / (server_metrics.samples + EXCLUDED.samples),
			net_tx_rate = (server_metrics.net_tx_rate * server_metrics.samples + EXCLUDED.net_tx_rate * EXCLUDED.samples)
			              / (server_metrics.samples + EXCLUDED.samples),
			samples     = server_metrics.samples + EXCLUDED.samples,
			mem_used = EXCLUDED.mem_used, mem_total = EXCLUDED.mem_total,
			disk_used = EXCLUDED.disk_used, disk_total = EXCLUDED.disk_total`,
		serverID, p.At.UTC().Truncate(time.Minute), samples, p.CPUPercent, p.Load1,
		p.MemUsed, p.MemTotal, p.DiskUsed, p.DiskTotal, p.NetRxRate, p.NetTxRate)
	return err
}

// CompactMetrics rolls minute buckets of the last two days up into hourly buckets, then
// deletes buckets past their retention. It is idempotent; the panel runs it every few minutes.
func (s *Store) CompactMetrics(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO server_metrics (server_id, resolution, bucket, samples, cpu_percent, load1,
			mem_used, mem_total, disk_used, disk_total, net_rx_rate, net_tx_rate)
		SELECT server_id, 3600, date_trunc('hour', bucket), sum(samples),
			sum(cpu_percent * samples) / sum(samples), sum(load1 * samples) / sum(samples),
			(array_agg(mem_used ORDER BY bucket DESC))[1], (array_agg(mem_total ORDER BY bucket DESC))[1],
			(array_agg(disk_used ORDER BY bucket DESC))[1], (array_agg(disk_total ORDER BY bucket DESC))[1],
			sum(net_rx_rate * samples) / sum(samples), sum(net_tx_rate * samples) / sum(samples)
		FROM server_metrics
		WHERE resolution = 60 AND bucket >= now() - $1 * interval '1 second'
		GROUP BY server_id, date_trunc('hour', bucket)
		ON CONFLICT (server_id, resolution, bucket) DO UPDATE SET
			samples = EXCLUDED.samples, cpu_percent = EXCLUDED.cpu_percent, load1 = EXCLUDED.load1,
			mem_used = EXCLUDED.mem_used, mem_total = EXCLUDED.mem_total,
			disk_used = EXCLUDED.disk_used, disk_total = EXCLUDED.disk_total,
			net_rx_rate = EXCLUDED.net_rx_rate, net_tx_rate = EXCLUDED.net_tx_rate`,
		int64(MinuteRetention.Seconds()))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		DELETE FROM server_metrics
		WHERE (resolution = 60 AND bucket < now() - $1 * interval '1 second')
		   OR (resolution = 3600 AND bucket < now() - $2 * interval '1 second')`,
		int64(MinuteRetention.Seconds()), int64(HourRetention.Seconds()))
	return err
}

// MetricRange returns points for the last `span`, averaged into `step`-sized buckets read
// from the given resolution (60 or 3600).
func (s *Store) MetricRange(ctx context.Context, serverID string, span, step time.Duration, resolution int) ([]MetricPoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT date_bin($3 * interval '1 second', bucket, TIMESTAMPTZ 'epoch') AS at,
			sum(cpu_percent * samples) / sum(samples), sum(load1 * samples) / sum(samples),
			max(mem_used), max(mem_total), max(disk_used), max(disk_total),
			sum(net_rx_rate * samples) / sum(samples), sum(net_tx_rate * samples) / sum(samples)
		FROM server_metrics
		WHERE server_id = $1 AND resolution = $4 AND bucket >= now() - $2 * interval '1 second'
		GROUP BY at ORDER BY at`,
		serverID, int64(span.Seconds()), int64(step.Seconds()), resolution)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MetricPoint, error) {
		var p MetricPoint
		err := r.Scan(&p.At, &p.CPUPercent, &p.Load1, &p.MemUsed, &p.MemTotal, &p.DiskUsed, &p.DiskTotal, &p.NetRxRate, &p.NetTxRate)
		return p, err
	})
}
