package store

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"ekpanel/internal/agentproto"
)

// RecordTraffic stores hourly totals from an agent. The agent sends whole hours, so a
// repeated hour replaces what was stored.
func (s *Store) RecordTraffic(ctx context.Context, buckets []agentproto.TrafficBucket) error {
	batch := &pgx.Batch{}
	for _, b := range buckets {
		col := "site_id"
		if b.Kind == "app" {
			col = "app_id"
		}
		// The row may belong to something deleted meanwhile; then nothing is stored.
		batch.Queue(`INSERT INTO traffic_hourly (`+col+`, hour, requests, bytes, s2xx, s3xx, s4xx, s5xx, paths)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9
			WHERE EXISTS (SELECT 1 FROM `+map[string]string{"site_id": "sites", "app_id": "apps"}[col]+` WHERE id = $1)
			ON CONFLICT (`+col+`, hour) WHERE `+col+` IS NOT NULL DO UPDATE SET requests = EXCLUDED.requests,
				bytes = EXCLUDED.bytes, s2xx = EXCLUDED.s2xx, s3xx = EXCLUDED.s3xx, s4xx = EXCLUDED.s4xx, s5xx = EXCLUDED.s5xx,
				paths = EXCLUDED.paths`,
			b.ID, b.Hour, b.Requests, b.Bytes, b.Status[0], b.Status[1], b.Status[2], b.Status[3], b.Paths)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

func (s *Store) PruneTraffic(ctx context.Context, keep time.Duration) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM traffic_hourly WHERE hour < now() - $1 * interval '1 second'`, int64(keep.Seconds()))
	return err
}

type TrafficPoint struct {
	At       time.Time `json:"at"`
	Requests int64     `json:"requests"`
	Bytes    int64     `json:"bytes"`
}

type TrafficPath struct {
	Path     string `json:"path"`
	Requests int64  `json:"requests"`
}

type Traffic struct {
	Points   []TrafficPoint `json:"points"`
	Requests int64          `json:"requests"`
	Bytes    int64          `json:"bytes"`
	Status   [4]int64       `json:"status"` // 2xx, 3xx, 4xx, 5xx
	Paths    []TrafficPath  `json:"paths"`
}

// TrafficOf sums a site's or app's traffic since `from`, in steps of `step` (an hour or a
// day, counted in UTC).
func (s *Store) TrafficOf(ctx context.Context, kind, id string, from time.Time, step time.Duration) (Traffic, error) {
	col := "site_id"
	if kind == "app" {
		col = "app_id"
	}
	t := Traffic{Points: []TrafficPoint{}, Paths: []TrafficPath{}}
	rows, err := s.pool.Query(ctx, `
		SELECT to_timestamp(floor(extract(epoch FROM hour) / $3) * $3) AS at, sum(requests), sum(bytes),
			sum(s2xx), sum(s3xx), sum(s4xx), sum(s5xx)
		FROM traffic_hourly WHERE `+col+` = $1 AND hour >= $2 GROUP BY at ORDER BY at`, id, from, int64(step.Seconds()))
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var p TrafficPoint
		var st [4]int64
		if err := rows.Scan(&p.At, &p.Requests, &p.Bytes, &st[0], &st[1], &st[2], &st[3]); err != nil {
			return t, err
		}
		t.Points = append(t.Points, p)
		t.Requests += p.Requests
		t.Bytes += p.Bytes
		for i := range st {
			t.Status[i] += st[i]
		}
	}
	if err := rows.Err(); err != nil {
		return t, err
	}
	paths := map[string]int64{}
	prows, err := s.pool.Query(ctx, `SELECT key, sum(value::bigint) FROM traffic_hourly, jsonb_each_text(paths)
		WHERE `+col+` = $1 AND hour >= $2 GROUP BY key`, id, from)
	if err != nil {
		return t, err
	}
	defer prows.Close()
	for prows.Next() {
		var p string
		var n int64
		if err := prows.Scan(&p, &n); err != nil {
			return t, err
		}
		paths[p] = n
	}
	for p, n := range paths {
		t.Paths = append(t.Paths, TrafficPath{Path: p, Requests: n})
	}
	sort.Slice(t.Paths, func(i, j int) bool {
		return t.Paths[i].Requests > t.Paths[j].Requests || (t.Paths[i].Requests == t.Paths[j].Requests && t.Paths[i].Path < t.Paths[j].Path)
	})
	if len(t.Paths) > 10 {
		t.Paths = t.Paths[:10]
	}
	return t, prows.Err()
}

// OwnerBandwidth is the bytes a customer's websites and apps served since `from`.
func (s *Store) OwnerBandwidth(ctx context.Context, ownerID string, from time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(t.bytes), 0) FROM traffic_hourly t
		LEFT JOIN sites st ON st.id = t.site_id LEFT JOIN apps ap ON ap.id = t.app_id
		WHERE COALESCE(st.owner_id, ap.owner_id) = $1 AND t.hour >= $2`, ownerID, from).Scan(&n)
	return n, err
}
