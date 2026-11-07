package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type CronJob struct {
	ID             string  `json:"id"`
	ServerID       string  `json:"server_id"`
	SiteID         *string `json:"site_id"`
	AppID          *string `json:"app_id"`
	Service        *string `json:"service"`
	Name           string  `json:"name"`
	Schedule       string  `json:"schedule"`
	Timezone       string  `json:"timezone"`
	Command        string  `json:"command"`
	TimeoutSeconds int     `json:"timeout_seconds"`
	Enabled        bool    `json:"enabled"`
	// From the website or app the job belongs to.
	TargetName  string  `json:"target_name"` // domain or app name
	LinuxUser   *string `json:"-"`
	OwnerID     *string `json:"owner_id"`
	TargetState string  `json:"-"`

	State             string    `json:"state"`
	Message           *string   `json:"message"`
	Generation        int64     `json:"generation"`
	AppliedGeneration int64     `json:"applied_generation"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

const cronColumns = `c.id, c.server_id, c.site_id, c.app_id, c.service, c.name, c.schedule, c.timezone, c.command,
	c.timeout_seconds, c.enabled, COALESCE(st.domain, ap.name), st.linux_user, COALESCE(st.owner_id, ap.owner_id),
	COALESCE(st.state, ap.state), c.state, c.message, c.generation, c.applied_generation, c.created_at, c.updated_at`

const cronFrom = ` FROM cron_jobs c LEFT JOIN sites st ON st.id = c.site_id LEFT JOIN apps ap ON ap.id = c.app_id`

func scanCron(row pgx.Row) (CronJob, error) {
	var c CronJob
	err := row.Scan(&c.ID, &c.ServerID, &c.SiteID, &c.AppID, &c.Service, &c.Name, &c.Schedule, &c.Timezone, &c.Command,
		&c.TimeoutSeconds, &c.Enabled, &c.TargetName, &c.LinuxUser, &c.OwnerID, &c.TargetState, &c.State, &c.Message,
		&c.Generation, &c.AppliedGeneration, &c.CreatedAt, &c.UpdatedAt)
	return c, notFound(err)
}

// CronSettings is everything about a job that can change.
type CronSettings struct {
	Name           string
	Schedule       string
	Timezone       string
	Command        string
	TimeoutSeconds int
	Enabled        bool
	Service        *string
}

type NewCronJob struct {
	ServerID  string
	SiteID    *string
	AppID     *string
	CreatedBy *string
	CronSettings
}

func (s *Store) CreateCronJob(ctx context.Context, n NewCronJob) (CronJob, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO cron_jobs (server_id, site_id, app_id, service, name, schedule, timezone, command, timeout_seconds, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		n.ServerID, n.SiteID, n.AppID, n.Service, n.Name, n.Schedule, n.Timezone, n.Command, n.TimeoutSeconds, n.Enabled,
		n.CreatedBy).Scan(&id)
	if err != nil {
		return CronJob{}, err
	}
	return s.GetCronJob(ctx, id)
}

func (s *Store) GetCronJob(ctx context.Context, id string) (CronJob, error) {
	return scanCron(s.pool.QueryRow(ctx, `SELECT `+cronColumns+cronFrom+` WHERE c.id = $1`, id))
}

// CronFilter narrows ListCronJobs; empty fields do not filter.
type CronFilter struct {
	ServerID, SiteID, AppID, OwnerID string
}

func (s *Store) ListCronJobs(ctx context.Context, f CronFilter) ([]CronJob, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cronColumns+cronFrom+`
		WHERE ($1 = '' OR c.server_id::text = $1) AND ($2 = '' OR c.site_id::text = $2) AND ($3 = '' OR c.app_id::text = $3)
		  AND ($4 = '' OR COALESCE(st.owner_id, ap.owner_id)::text = $4)
		ORDER BY c.name, c.created_at`, f.ServerID, f.SiteID, f.AppID, f.OwnerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CronJob, error) { return scanCron(r) })
}

func (s *Store) UpdateCronJob(ctx context.Context, id string, c CronSettings) (CronJob, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cron_jobs SET name = $2, schedule = $3, timezone = $4, command = $5, timeout_seconds = $6, enabled = $7,
			service = $8, generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1 AND state <> 'deleting'`,
		id, c.Name, c.Schedule, c.Timezone, c.Command, c.TimeoutSeconds, c.Enabled, c.Service)
	if err != nil {
		return CronJob{}, err
	}
	if tag.RowsAffected() == 0 {
		return CronJob{}, ErrNotFound
	}
	return s.GetCronJob(ctx, id)
}

func (s *Store) MarkCronDeleting(ctx context.Context, id string) (CronJob, error) {
	_, err := s.pool.Exec(ctx, `
		UPDATE cron_jobs SET state = 'deleting', message = NULL, generation = generation + 1, updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return CronJob{}, err
	}
	return s.GetCronJob(ctx, id)
}

// ApplyCronResult records what the agent reported for one job at a given generation.
func (s *Store) ApplyCronResult(ctx context.Context, id string, generation int64, ok bool, message string) error {
	if ok {
		tag, err := s.pool.Exec(ctx, `DELETE FROM cron_jobs WHERE id = $1 AND state = 'deleting' AND generation = $2`, id, generation)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			UPDATE cron_jobs SET state = 'active', message = NULL, applied_generation = $2
			WHERE id = $1 AND generation = $2 AND state <> 'deleting'`, id, generation)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE cron_jobs SET message = $3, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE id = $1 AND generation = $2`, id, generation, message)
	return err
}

func (s *Store) FailUnappliedCron(ctx context.Context, serverID, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE cron_jobs SET message = $2, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE server_id = $1 AND generation > applied_generation`, serverID, message)
	return err
}
