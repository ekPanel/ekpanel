package store

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ResellerLimits is what an administrator gives a reseller. Counts, disk space and bandwidth
// are totals over all the reseller's customers; memory, CPU and the mailbox size are the
// most one package may give; the lists and switches are what packages may offer at all.
type ResellerLimits struct {
	MaxCustomers   int      `json:"max_customers"`
	MaxSites       int      `json:"max_sites"`
	MaxDatabases   int      `json:"max_databases"`
	MaxApps        int      `json:"max_apps"`
	MaxCronJobs    int      `json:"max_cron_jobs"`
	MaxMailDomains int      `json:"max_mail_domains"`
	MaxMailboxes   int      `json:"max_mailboxes"`
	DiskMB         *int     `json:"disk_mb"`
	BandwidthGB    *int     `json:"bandwidth_gb"`
	BandwidthStop  bool     `json:"bandwidth_stop"`
	MemoryMB       *int     `json:"memory_mb"`
	CPUPercent     *int     `json:"cpu_percent"`
	MailboxQuotaMB *int     `json:"mailbox_quota_mb"`
	PHPVersions    []string `json:"php_versions"`
	AppImages      []string `json:"app_images"`
	ServerIDs      []string `json:"server_ids"`
	SFTP           bool     `json:"sftp"`
	FTP            bool     `json:"ftp"`
	GitApps        bool     `json:"git_apps"`
	// WhiteLabel lets the reseller give their panel their own name, logo, color and domain.
	WhiteLabel bool `json:"white_label"`
}

type Reseller struct {
	User
	Limits ResellerLimits `json:"limits"`
	// What all the reseller's customers have together.
	Usage     Usage `json:"usage"`
	Customers int   `json:"customers"`
	Packages  int   `json:"packages"`
	// Bytes served by the customers' websites and apps this month (UTC).
	BandwidthMonth   int64   `json:"bandwidth_month"`
	BandwidthBlocked bool    `json:"bandwidth_blocked"`
	SuspendReason    *string `json:"suspend_reason"`
}

const resellerLimitColumns = `max_customers, max_sites, max_databases, max_apps, max_cron_jobs, max_mail_domains, max_mailboxes,
	disk_mb, bandwidth_gb, bandwidth_stop, memory_mb, cpu_percent, mailbox_quota_mb, php_versions, app_images,
	server_ids::text[], sftp, ftp, git_apps, white_label`

func scanResellerLimits(row pgx.Row) (ResellerLimits, error) {
	var l ResellerLimits
	err := row.Scan(&l.MaxCustomers, &l.MaxSites, &l.MaxDatabases, &l.MaxApps, &l.MaxCronJobs, &l.MaxMailDomains, &l.MaxMailboxes,
		&l.DiskMB, &l.BandwidthGB, &l.BandwidthStop, &l.MemoryMB, &l.CPUPercent, &l.MailboxQuotaMB, &l.PHPVersions, &l.AppImages,
		&l.ServerIDs, &l.SFTP, &l.FTP, &l.GitApps, &l.WhiteLabel)
	return l, notFound(err)
}

func resellerLimitArgs(l ResellerLimits) []any {
	return []any{l.MaxCustomers, l.MaxSites, l.MaxDatabases, l.MaxApps, l.MaxCronJobs, l.MaxMailDomains, l.MaxMailboxes,
		l.DiskMB, l.BandwidthGB, l.BandwidthStop, l.MemoryMB, l.CPUPercent, l.MailboxQuotaMB, l.PHPVersions, l.AppImages,
		l.ServerIDs, l.SFTP, l.FTP, l.GitApps, l.WhiteLabel}
}

// GetResellerLimits returns a reseller's limits.
func (s *Store) GetResellerLimits(ctx context.Context, id string) (ResellerLimits, error) {
	return scanResellerLimits(s.pool.QueryRow(ctx, `SELECT `+resellerLimitColumns+` FROM resellers WHERE user_id = $1`, id))
}

// resellerUsageSQL is usageSQL over all the customers of the reseller $1.
var resellerUsageSQL = strings.ReplaceAll(usageSQL, "= $1", "IN (SELECT id FROM users WHERE reseller_id = $1)")

// ResellerUsage is what all of a reseller's customers have together.
func (s *Store) ResellerUsage(ctx context.Context, id string) (Usage, error) {
	var u Usage
	err := s.pool.QueryRow(ctx, `SELECT `+resellerUsageSQL, id).Scan(&u.Sites, &u.Databases, &u.Apps, &u.CronJobs, &u.MailDomains,
		&u.Mailboxes, &u.DiskBytes)
	return u, err
}

// ResellerBandwidth is what a reseller's customers' websites and apps served since from.
func (s *Store) ResellerBandwidth(ctx context.Context, id string, from time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(t.bytes), 0) FROM traffic_hourly t
		LEFT JOIN sites st ON st.id = t.site_id LEFT JOIN apps ap ON ap.id = t.app_id
		WHERE COALESCE(st.owner_id, ap.owner_id) IN (SELECT id FROM users WHERE reseller_id = $1) AND t.hour >= $2`, id, from).Scan(&n)
	return n, err
}

func (s *Store) GetReseller(ctx context.Context, id string) (Reseller, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1 AND role = 'reseller'`, id))
	if err != nil {
		return Reseller{}, err
	}
	r := Reseller{User: u}
	if r.Limits, err = s.GetResellerLimits(ctx, id); err != nil {
		return r, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT suspend_reason, bandwidth_blocked,
			(SELECT count(*) FROM users WHERE reseller_id = $1), (SELECT count(*) FROM packages WHERE reseller_id = $1)
		FROM users WHERE id = $1`, id).Scan(&r.SuspendReason, &r.BandwidthBlocked, &r.Customers, &r.Packages); err != nil {
		return r, err
	}
	if r.Usage, err = s.ResellerUsage(ctx, id); err != nil {
		return r, err
	}
	now := time.Now().UTC()
	r.BandwidthMonth, err = s.ResellerBandwidth(ctx, id, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	return r, err
}

func (s *Store) ListResellers(ctx context.Context) ([]Reseller, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM users WHERE role = 'reseller' ORDER BY lower(name), email`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	list := make([]Reseller, 0, len(ids))
	for _, id := range ids {
		r, err := s.GetReseller(ctx, id)
		if err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, nil
}

type NewReseller struct {
	Email        string
	Name         string
	PasswordHash string
	Locale       string
	Limits       ResellerLimits
}

func (s *Store) CreateReseller(ctx context.Context, n NewReseller) (Reseller, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reseller{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO users (email, name, password_hash, role, locale) VALUES ($1, $2, $3, 'reseller', $4) RETURNING id`,
		n.Email, n.Name, n.PasswordHash, n.Locale).Scan(&id)
	if isUniqueViolation(err) {
		return Reseller{}, ErrDuplicateEmail
	}
	if err != nil {
		return Reseller{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO resellers (user_id, max_customers, max_sites, max_databases, max_apps, max_cron_jobs,
			max_mail_domains, max_mailboxes, disk_mb, bandwidth_gb, bandwidth_stop, memory_mb, cpu_percent, mailbox_quota_mb,
			php_versions, app_images, server_ids, sftp, ftp, git_apps, white_label)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17::text[]::uuid[], $18, $19, $20, $21)`,
		append([]any{id}, resellerLimitArgs(n.Limits)...)...); err != nil {
		return Reseller{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reseller{}, err
	}
	return s.GetReseller(ctx, id)
}

func (s *Store) UpdateReseller(ctx context.Context, id, email, name string, l ResellerLimits) (Reseller, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reseller{}, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE users SET email = $2, name = $3, updated_at = now() WHERE id = $1 AND role = 'reseller'`, id, email, name)
	if isUniqueViolation(err) {
		return Reseller{}, ErrDuplicateEmail
	}
	if err != nil {
		return Reseller{}, err
	}
	if tag.RowsAffected() == 0 {
		return Reseller{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE resellers SET max_customers = $2, max_sites = $3, max_databases = $4, max_apps = $5,
			max_cron_jobs = $6, max_mail_domains = $7, max_mailboxes = $8, disk_mb = $9, bandwidth_gb = $10, bandwidth_stop = $11,
			memory_mb = $12, cpu_percent = $13, mailbox_quota_mb = $14, php_versions = $15, app_images = $16,
			server_ids = $17::text[]::uuid[], sftp = $18, ftp = $19, git_apps = $20, white_label = $21, updated_at = now()
		WHERE user_id = $1`, append([]any{id}, resellerLimitArgs(l)...)...); err != nil {
		return Reseller{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reseller{}, err
	}
	return s.GetReseller(ctx, id)
}

// DeleteReseller removes a reseller who has no customers any more, with their packages.
func (s *Store) DeleteReseller(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1 AND role = 'reseller'`, id)
	if isForeignKeyViolation(err) {
		return ErrInUse
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ResellerOf returns the reseller whose customer the user is, or nil.
func (s *Store) ResellerOf(ctx context.Context, userID string) (*string, error) {
	var id *string
	err := s.pool.QueryRow(ctx, `SELECT reseller_id::text FROM users WHERE id = $1`, userID).Scan(&id)
	return id, notFound(err)
}

// CustomersOfReseller lists the IDs of a reseller's customers.
func (s *Store) CustomersOfReseller(ctx context.Context, id string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM users WHERE reseller_id = $1`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// BandwidthResellers lists the resellers with a bandwidth limit, and those still marked as
// blocked.
func (s *Store) BandwidthResellers(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id::text FROM users u JOIN resellers r ON r.user_id = u.id
		WHERE r.bandwidth_gb IS NOT NULL OR u.bandwidth_blocked`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ownedBy is an SQL condition: the owner column col is the user p, or one of the customers
// of the reseller p.
func ownedBy(col, p string) string {
	return `(` + col + `::text = ` + p + ` OR ` + col + ` IN (SELECT id FROM users WHERE reseller_id = NULLIF(` + p + `, '')::uuid))`
}
