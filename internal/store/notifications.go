package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- Settings ----

func (s *Store) GetSetting(ctx context.Context, key string, v any) (bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&raw)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

func (s *Store) SetSetting(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, raw)
	return err
}

// ---- Channels ----

type Channel struct {
	ID         string            `json:"id"`
	UserID     string            `json:"user_id"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Config     map[string]string `json:"config"`
	Secret     []byte            `json:"-"`
	Events     []string          `json:"events"`
	Enabled    bool              `json:"enabled"`
	LastSentAt *time.Time        `json:"last_sent_at"`
	LastError  *string           `json:"last_error"`
	CreatedAt  time.Time         `json:"created_at"`
}

const channelColumns = `id, user_id, kind, name, config, secret, events, enabled, last_sent_at, last_error, created_at`

func scanChannel(row pgx.Row) (Channel, error) {
	var c Channel
	err := row.Scan(&c.ID, &c.UserID, &c.Kind, &c.Name, &c.Config, &c.Secret, &c.Events, &c.Enabled, &c.LastSentAt, &c.LastError, &c.CreatedAt)
	if c.Config == nil {
		c.Config = map[string]string{}
	}
	return c, notFound(err)
}

func (s *Store) ListChannels(ctx context.Context, userID string) ([]Channel, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Channel, error) { return scanChannel(r) })
}

func (s *Store) GetChannel(ctx context.Context, id string) (Channel, error) {
	return scanChannel(s.pool.QueryRow(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = $1`, id))
}

// SaveChannel creates a channel (empty ID) or replaces one. A nil secret keeps the old one.
func (s *Store) SaveChannel(ctx context.Context, c Channel) (Channel, error) {
	if c.Events == nil {
		c.Events = []string{}
	}
	if c.ID == "" {
		return scanChannel(s.pool.QueryRow(ctx, `INSERT INTO notification_channels (user_id, kind, name, config, secret, events, enabled)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+channelColumns, c.UserID, c.Kind, c.Name, c.Config, c.Secret, c.Events, c.Enabled))
	}
	return scanChannel(s.pool.QueryRow(ctx, `UPDATE notification_channels SET name = $2, config = $3, secret = COALESCE($4, secret),
			events = $5, enabled = $6, last_error = NULL
		WHERE id = $1 RETURNING `+channelColumns, c.ID, c.Name, c.Config, c.Secret, c.Events, c.Enabled))
}

func (s *Store) DeleteChannel(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM notification_channels WHERE id = $1`, id)
	return err
}

func (s *Store) ChannelResult(ctx context.Context, id string, sendErr error) error {
	if sendErr == nil {
		_, err := s.pool.Exec(ctx, `UPDATE notification_channels SET last_sent_at = now(), last_error = NULL WHERE id = $1`, id)
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE notification_channels SET last_error = $2 WHERE id = $1`, id, sendErr.Error())
	return err
}

// ---- Notifications ----

type Notification struct {
	ID        int64             `json:"id"`
	UserID    string            `json:"user_id"`
	Event     string            `json:"event"`
	Severity  string            `json:"severity"`
	Params    map[string]string `json:"params"`
	Link      string            `json:"link"`
	CreatedAt time.Time         `json:"created_at"`
	ReadAt    *time.Time        `json:"read_at"`
}

const notificationColumns = `id, user_id, event, severity, params, link, created_at, read_at`

func scanNotification(row pgx.Row) (Notification, error) {
	var n Notification
	err := row.Scan(&n.ID, &n.UserID, &n.Event, &n.Severity, &n.Params, &n.Link, &n.CreatedAt, &n.ReadAt)
	return n, notFound(err)
}

// AddNotification stores a notification for a user and queues it for each of their enabled
// channels that wants this event.
func (s *Store) AddNotification(ctx context.Context, n Notification) (Notification, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return n, err
	}
	defer tx.Rollback(ctx)
	if n.Params == nil {
		n.Params = map[string]string{}
	}
	n, err = scanNotification(tx.QueryRow(ctx, `INSERT INTO notifications (user_id, event, severity, params, link)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+notificationColumns, n.UserID, n.Event, n.Severity, n.Params, n.Link))
	if err != nil {
		return n, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO notification_deliveries (notification_id, channel_id)
		SELECT $1, id FROM notification_channels
		WHERE user_id = $2 AND enabled AND (cardinality(events) = 0 OR $3 = ANY(events))`, n.ID, n.UserID, n.Event)
	if err != nil {
		return n, err
	}
	return n, tx.Commit(ctx)
}

func (s *Store) ListNotifications(ctx context.Context, userID string, limit int) ([]Notification, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	rows, err := s.pool.Query(ctx, `SELECT `+notificationColumns+` FROM notifications WHERE user_id = $1
		ORDER BY created_at DESC, id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Notification, error) { return scanNotification(r) })
}

func (s *Store) UnreadNotifications(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&n)
	return n, err
}

// MarkNotificationsRead marks the given notifications, or all with ids nil, as read.
func (s *Store) MarkNotificationsRead(ctx context.Context, userID string, ids []int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE notifications SET read_at = now()
		WHERE user_id = $1 AND read_at IS NULL AND ($2::bigint[] IS NULL OR id = ANY($2))`, userID, ids)
	return err
}

func (s *Store) PruneNotifications(ctx context.Context, keep time.Duration) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM notifications WHERE created_at < now() - $1 * interval '1 second'`, int64(keep.Seconds()))
	return err
}

// Delivery is one notification due to go out to one channel.
type Delivery struct {
	Notification Notification
	Channel      Channel
	Attempts     int
	Locale       string
}

// DueDeliveries takes up to `limit` deliveries that are due and pushes their next attempt
// back by `lease`, so another sender does not take them meanwhile.
func (s *Store) DueDeliveries(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT notification_id, channel_id FROM notification_deliveries
			WHERE NOT done AND next_at <= now() ORDER BY next_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		UPDATE notification_deliveries d SET next_at = now() + $2 * interval '1 second', attempts = d.attempts + 1
		FROM due WHERE d.notification_id = due.notification_id AND d.channel_id = due.channel_id
		RETURNING d.notification_id, d.channel_id, d.attempts`, limit, int64(lease.Seconds()))
	if err != nil {
		return nil, err
	}
	type key struct {
		n        int64
		c        string
		attempts int
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (key, error) {
		var k key
		return k, r.Scan(&k.n, &k.c, &k.attempts)
	})
	if err != nil {
		return nil, err
	}
	out := make([]Delivery, 0, len(keys))
	for _, k := range keys {
		var d Delivery
		if d.Notification, err = scanNotification(s.pool.QueryRow(ctx, `SELECT `+notificationColumns+` FROM notifications WHERE id = $1`, k.n)); err != nil {
			continue
		}
		if d.Channel, err = s.GetChannel(ctx, k.c); err != nil {
			continue
		}
		s.pool.QueryRow(ctx, `SELECT locale FROM users WHERE id = $1`, d.Notification.UserID).Scan(&d.Locale)
		d.Attempts = k.attempts
		out = append(out, d)
	}
	return out, nil
}

// FinishDelivery records a delivery's outcome; a failure is tried again at retryAt, or
// given up with a zero retryAt.
func (s *Store) FinishDelivery(ctx context.Context, notificationID int64, channelID string, sendErr error, retryAt time.Time) error {
	if sendErr == nil {
		_, err := s.pool.Exec(ctx, `UPDATE notification_deliveries SET done = true, error = NULL WHERE notification_id = $1 AND channel_id = $2`,
			notificationID, channelID)
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE notification_deliveries SET done = $3, error = $4, next_at = COALESCE($5, next_at)
		WHERE notification_id = $1 AND channel_id = $2`, notificationID, channelID, retryAt.IsZero(), sendErr.Error(), nullTime(retryAt))
	return err
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ---- Alert state ----

// SwapAlertState stores a condition's new value and returns the previous one ("" if none).
func (s *Store) SwapAlertState(ctx context.Context, key, value string) (string, error) {
	var prev *string
	err := s.pool.QueryRow(ctx, `
		WITH old AS (SELECT value FROM alert_state WHERE key = $1)
		INSERT INTO alert_state (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
		RETURNING (SELECT value FROM old)`, key, value).Scan(&prev)
	if err != nil || prev == nil {
		return "", err
	}
	return *prev, nil
}

// AdminIDs lists the administrators, who receive the panel's own alerts.
func (s *Store) AdminIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM users WHERE role = 'admin'`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// LoggedInFrom reports whether a user signed in from an IP address in the last 90 days.
func (s *Store) LoggedInFrom(ctx context.Context, userID, ip string) (seen, any bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM audit_log WHERE actor_id = $1 AND action = 'auth.login' AND actor_ip = $2 AND at > now() - interval '90 days'),
			EXISTS (SELECT 1 FROM audit_log WHERE actor_id = $1 AND action = 'auth.login')`, userID, ip).Scan(&seen, &any)
	return seen, any, err
}
