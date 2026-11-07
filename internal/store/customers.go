package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrInUse means the row is still referenced: a package with customers, or a customer who
// still owns websites, databases or apps.
var ErrInUse = errors.New("still in use")

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// ---- Packages ----

// PackageLimits is everything a package defines. Nil numbers mean no limit.
type PackageLimits struct {
	Name         string   `json:"name"`
	MaxSites     int      `json:"max_sites"`
	MaxDatabases int      `json:"max_databases"`
	MaxApps      int      `json:"max_apps"`
	MaxCronJobs  int      `json:"max_cron_jobs"`
	DiskMB       *int     `json:"disk_mb"`
	MemoryMB     *int     `json:"memory_mb"`
	CPUPercent   *int     `json:"cpu_percent"`
	PHPVersions  []string `json:"php_versions"`
	AppImages    []string `json:"app_images"`
	ServerIDs    []string `json:"server_ids"`
	SFTP         bool     `json:"sftp"`
}

type Package struct {
	ID string `json:"id"`
	PackageLimits
	Customers int       `json:"customers"`
	CreatedAt time.Time `json:"created_at"`
}

const packageColumns = `p.id, p.name, p.max_sites, p.max_databases, p.max_apps, p.max_cron_jobs, p.disk_mb, p.memory_mb, p.cpu_percent,
	p.php_versions, p.app_images, p.server_ids::text[], p.sftp, p.created_at,
	(SELECT count(*) FROM users u WHERE u.package_id = p.id)`

func scanPackage(row pgx.Row) (Package, error) {
	var p Package
	err := row.Scan(&p.ID, &p.Name, &p.MaxSites, &p.MaxDatabases, &p.MaxApps, &p.MaxCronJobs, &p.DiskMB, &p.MemoryMB, &p.CPUPercent,
		&p.PHPVersions, &p.AppImages, &p.ServerIDs, &p.SFTP, &p.CreatedAt, &p.Customers)
	return p, notFound(err)
}

// ErrPackageNameTaken means another package already has that name.
var ErrPackageNameTaken = errors.New("a package with this name already exists")

func (s *Store) CreatePackage(ctx context.Context, l PackageLimits) (Package, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO packages (name, max_sites, max_databases, max_apps, disk_mb, memory_mb, cpu_percent, php_versions,
			app_images, server_ids, sftp, max_cron_jobs)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text[]::uuid[], $11, $12) RETURNING id`,
		l.Name, l.MaxSites, l.MaxDatabases, l.MaxApps, l.DiskMB, l.MemoryMB, l.CPUPercent, l.PHPVersions, l.AppImages,
		l.ServerIDs, l.SFTP, l.MaxCronJobs).Scan(&id)
	if isUniqueViolation(err) {
		return Package{}, ErrPackageNameTaken
	}
	if err != nil {
		return Package{}, err
	}
	return s.GetPackage(ctx, id)
}

func (s *Store) GetPackage(ctx context.Context, id string) (Package, error) {
	return scanPackage(s.pool.QueryRow(ctx, `SELECT `+packageColumns+` FROM packages p WHERE p.id = $1`, id))
}

func (s *Store) ListPackages(ctx context.Context) ([]Package, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+packageColumns+` FROM packages p ORDER BY lower(p.name)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Package, error) { return scanPackage(r) })
}

// UpdatePackage changes a package; its customers get the new limits at the next apply.
func (s *Store) UpdatePackage(ctx context.Context, id string, l PackageLimits) (Package, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE packages SET name = $2, max_sites = $3, max_databases = $4, max_apps = $5, disk_mb = $6, memory_mb = $7,
			cpu_percent = $8, php_versions = $9, app_images = $10, server_ids = $11::text[]::uuid[], sftp = $12,
			max_cron_jobs = $13, updated_at = now()
		WHERE id = $1`,
		id, l.Name, l.MaxSites, l.MaxDatabases, l.MaxApps, l.DiskMB, l.MemoryMB, l.CPUPercent, l.PHPVersions, l.AppImages,
		l.ServerIDs, l.SFTP, l.MaxCronJobs)
	if isUniqueViolation(err) {
		return Package{}, ErrPackageNameTaken
	}
	if err != nil {
		return Package{}, err
	}
	if tag.RowsAffected() == 0 {
		return Package{}, ErrNotFound
	}
	return s.GetPackage(ctx, id)
}

func (s *Store) DeletePackage(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM packages WHERE id = $1`, id)
	if isForeignKeyViolation(err) {
		return ErrInUse
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---- Customers ----

// Usage is what a customer has, against their package.
type Usage struct {
	Sites     int   `json:"sites"`
	Databases int   `json:"databases"`
	Apps      int   `json:"apps"`
	CronJobs  int   `json:"cron_jobs"`
	DiskBytes int64 `json:"disk_bytes"` // files of sites, app volumes and folders, databases
}

type Customer struct {
	User
	PackageName string `json:"package_name"`
	Usage       Usage  `json:"usage"`
}

// usageSQL measures the owner $1. Resources being deleted no longer count.
const usageSQL = `
	(SELECT count(*) FROM sites WHERE owner_id = $1 AND state <> 'deleting'),
	(SELECT count(*) FROM databases WHERE owner_id = $1 AND state <> 'deleting'),
	(SELECT count(*) FROM apps WHERE owner_id = $1 AND state <> 'deleting'),
	(SELECT count(*) FROM cron_jobs c LEFT JOIN sites st ON st.id = c.site_id LEFT JOIN apps ap ON ap.id = c.app_id
	 WHERE COALESCE(st.owner_id, ap.owner_id) = $1 AND c.state <> 'deleting'),
	COALESCE((SELECT sum(disk_bytes) FROM sites WHERE owner_id = $1 AND state <> 'deleting'), 0) +
	COALESCE((SELECT sum(size_bytes) FROM databases WHERE owner_id = $1 AND state <> 'deleting'), 0) +
	COALESCE((SELECT sum(disk_bytes) FROM apps WHERE owner_id = $1 AND state <> 'deleting'), 0)`

func (s *Store) OwnerUsage(ctx context.Context, ownerID string) (Usage, error) {
	var u Usage
	err := s.pool.QueryRow(ctx, `SELECT `+usageSQL, ownerID).Scan(&u.Sites, &u.Databases, &u.Apps, &u.CronJobs, &u.DiskBytes)
	return u, err
}

func (s *Store) ListCustomers(ctx context.Context) ([]Customer, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id FROM users u WHERE u.role = 'customer' ORDER BY lower(u.name), u.email`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	list := make([]Customer, 0, len(ids))
	for _, id := range ids {
		c, err := s.GetCustomer(ctx, id)
		if err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, nil
}

func (s *Store) GetCustomer(ctx context.Context, id string) (Customer, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1 AND role = 'customer'`, id))
	if err != nil {
		return Customer{}, err
	}
	c := Customer{User: u}
	if err := s.pool.QueryRow(ctx, `SELECT name FROM packages WHERE id = $1`, u.PackageID).Scan(&c.PackageName); err != nil {
		return c, err
	}
	c.Usage, err = s.OwnerUsage(ctx, id)
	return c, err
}

type NewCustomer struct {
	Email        string
	Name         string
	PasswordHash string
	Locale       string
	PackageID    string
}

func (s *Store) CreateCustomer(ctx context.Context, n NewCustomer) (Customer, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (email, name, password_hash, role, locale, package_id)
		VALUES ($1, $2, $3, 'customer', $4, $5) RETURNING id`,
		n.Email, n.Name, n.PasswordHash, n.Locale, n.PackageID).Scan(&id)
	if isUniqueViolation(err) {
		return Customer{}, ErrDuplicateEmail
	}
	if isForeignKeyViolation(err) {
		return Customer{}, ErrNotFound
	}
	if err != nil {
		return Customer{}, err
	}
	return s.GetCustomer(ctx, id)
}

func (s *Store) UpdateCustomer(ctx context.Context, id, email, name, packageID string) (Customer, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE users SET email = $2, name = $3, package_id = $4, updated_at = now() WHERE id = $1 AND role = 'customer'`,
		id, email, name, packageID)
	if isUniqueViolation(err) {
		return Customer{}, ErrDuplicateEmail
	}
	if isForeignKeyViolation(err) {
		return Customer{}, ErrNotFound
	}
	if err != nil {
		return Customer{}, err
	}
	if tag.RowsAffected() == 0 {
		return Customer{}, ErrNotFound
	}
	return s.GetCustomer(ctx, id)
}

// SetPassword replaces a user's password and signs them out everywhere.
func (s *Store) SetPassword(ctx context.Context, id, hash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, hash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteCustomer removes a customer who owns nothing any more.
func (s *Store) DeleteCustomer(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1 AND role = 'customer'`, id)
	if isForeignKeyViolation(err) {
		return ErrInUse
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---- Disk use ----

// RecordDiskUse stores what the agent measured for sites and apps.
func (s *Store) RecordDiskUse(ctx context.Context, sites, apps map[string]int64) error {
	batch := &pgx.Batch{}
	for id, b := range sites {
		batch.Queue(`UPDATE sites SET disk_bytes = $2 WHERE id = $1`, id, b)
	}
	for id, b := range apps {
		batch.Queue(`UPDATE apps SET disk_bytes = $2 WHERE id = $1`, id, b)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// ServersOfOwner lists the servers where a customer has a website, database or app.
func (s *Store) ServersOfOwner(ctx context.Context, ownerID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT server_id::text FROM sites WHERE owner_id = $1
		UNION SELECT server_id::text FROM databases WHERE owner_id = $1
		UNION SELECT server_id::text FROM apps WHERE owner_id = $1`, ownerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ServersOfPackage lists the servers where any customer of a package has something.
func (s *Store) ServersOfPackage(ctx context.Context, packageID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT x.server_id::text FROM users u JOIN (
			SELECT owner_id, server_id FROM sites UNION SELECT owner_id, server_id FROM databases
			UNION SELECT owner_id, server_id FROM apps) x ON x.owner_id = u.id
		WHERE u.package_id = $1`, packageID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
