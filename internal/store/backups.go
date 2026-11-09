package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type BackupRepo struct {
	ID          string     `json:"id"`
	ServerID    string     `json:"server_id"`
	ServerName  string     `json:"server_name"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	Location    string     `json:"location"`
	Schedule    string     `json:"schedule"`
	HourUTC     int        `json:"hour_utc"`
	KeepDaily   int        `json:"keep_daily"`
	KeepWeekly  int        `json:"keep_weekly"`
	KeepMonthly int        `json:"keep_monthly"`
	NextRunAt   *time.Time `json:"next_run_at"`
	LastRunAt   *time.Time `json:"last_run_at"`
	LastStatus  *string    `json:"last_status"`
	LastMessage *string    `json:"last_message"`
	CreatedAt   time.Time  `json:"created_at"`
}

const backupRepoColumns = `b.id, b.server_id, sv.name, b.name, b.kind, b.location, b.schedule, b.hour_utc,
	b.keep_daily, b.keep_weekly, b.keep_monthly, b.next_run_at, b.last_run_at, b.last_status, b.last_message, b.created_at`

const backupRepoFrom = ` FROM backup_repos b JOIN servers sv ON sv.id = b.server_id`

func scanBackupRepo(row pgx.Row) (BackupRepo, error) {
	var b BackupRepo
	err := row.Scan(&b.ID, &b.ServerID, &b.ServerName, &b.Name, &b.Kind, &b.Location, &b.Schedule, &b.HourUTC,
		&b.KeepDaily, &b.KeepWeekly, &b.KeepMonthly, &b.NextRunAt, &b.LastRunAt, &b.LastStatus, &b.LastMessage, &b.CreatedAt)
	return b, notFound(err)
}

// BackupSchedule is the part of a repository a user can change after creating it.
type BackupSchedule struct {
	Schedule    string `json:"schedule"`
	HourUTC     int    `json:"hour_utc"`
	KeepDaily   int    `json:"keep_daily"`
	KeepWeekly  int    `json:"keep_weekly"`
	KeepMonthly int    `json:"keep_monthly"`
}

// NextBackupRun is the first scheduled time after `after`: on the hour for hourly, at
// HourUTC for daily, and on Sundays at HourUTC for weekly. Off returns nil.
func NextBackupRun(s BackupSchedule, after time.Time) *time.Time {
	after = after.UTC()
	var next time.Time
	switch s.Schedule {
	case "hourly":
		next = after.Truncate(time.Hour).Add(time.Hour)
	case "daily":
		next = time.Date(after.Year(), after.Month(), after.Day(), s.HourUTC, 0, 0, 0, time.UTC)
		if !next.After(after) {
			next = next.AddDate(0, 0, 1)
		}
	case "weekly":
		next = time.Date(after.Year(), after.Month(), after.Day(), s.HourUTC, 0, 0, 0, time.UTC)
		for next.Weekday() != time.Sunday || !next.After(after) {
			next = next.AddDate(0, 0, 1)
		}
	default:
		return nil
	}
	return &next
}

type NewBackupRepo struct {
	ID        string // chosen by the panel before setup, so the agent can name its files
	ServerID  string
	Name      string
	Kind      string
	Location  string
	Schedule  BackupSchedule
	CreatedBy *string
}

func (s *Store) CreateBackupRepo(ctx context.Context, n NewBackupRepo) (BackupRepo, error) {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO backup_repos (id, server_id, name, kind, location, schedule, hour_utc, keep_daily, keep_weekly, keep_monthly, next_run_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		n.ID, n.ServerID, n.Name, n.Kind, n.Location, n.Schedule.Schedule, n.Schedule.HourUTC,
		n.Schedule.KeepDaily, n.Schedule.KeepWeekly, n.Schedule.KeepMonthly, NextBackupRun(n.Schedule, time.Now()), n.CreatedBy)
	if err != nil {
		return BackupRepo{}, err
	}
	return s.GetBackupRepo(ctx, n.ID)
}

func (s *Store) GetBackupRepo(ctx context.Context, id string) (BackupRepo, error) {
	return scanBackupRepo(s.pool.QueryRow(ctx, `SELECT `+backupRepoColumns+backupRepoFrom+` WHERE b.id = $1`, id))
}

func (s *Store) ListBackupRepos(ctx context.Context) ([]BackupRepo, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+backupRepoColumns+backupRepoFrom+` ORDER BY sv.name, b.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (BackupRepo, error) { return scanBackupRepo(r) })
}

func (s *Store) UpdateBackupSchedule(ctx context.Context, id string, sc BackupSchedule) (BackupRepo, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE backup_repos SET schedule = $2, hour_utc = $3, keep_daily = $4, keep_weekly = $5, keep_monthly = $6, next_run_at = $7
		WHERE id = $1`, id, sc.Schedule, sc.HourUTC, sc.KeepDaily, sc.KeepWeekly, sc.KeepMonthly, NextBackupRun(sc, time.Now()))
	if err != nil {
		return BackupRepo{}, err
	}
	if tag.RowsAffected() == 0 {
		return BackupRepo{}, ErrNotFound
	}
	return s.GetBackupRepo(ctx, id)
}

func (s *Store) DeleteBackupRepo(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM backup_repos WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// DueBackupRepos returns repositories whose next scheduled run has come, and moves each
// one's next run forward in the same statement so no run is started twice.
func (s *Store) DueBackupRepos(ctx context.Context) ([]BackupRepo, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+backupRepoColumns+backupRepoFrom+`
		WHERE b.schedule <> 'off' AND b.next_run_at <= now() FOR UPDATE OF b SKIP LOCKED`)
	if err != nil {
		return nil, err
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BackupRepo, error) { return scanBackupRepo(r) })
	if err != nil {
		return nil, err
	}
	for _, b := range due {
		next := NextBackupRun(BackupSchedule{Schedule: b.Schedule, HourUTC: b.HourUTC}, time.Now())
		if _, err := tx.Exec(ctx, `UPDATE backup_repos SET next_run_at = $2 WHERE id = $1`, b.ID, next); err != nil {
			return nil, err
		}
	}
	return due, tx.Commit(ctx)
}

func (s *Store) RecordBackupRun(ctx context.Context, id, status, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE backup_repos SET last_run_at = now(), last_status = $2, last_message = NULLIF($3, '') WHERE id = $1`, id, status, message)
	return err
}
