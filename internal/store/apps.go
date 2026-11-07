package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EnvVar is one environment variable of an app.
type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type App struct {
	ID                string    `json:"id"`
	ServerID          string    `json:"server_id"`
	ServerName        string    `json:"server_name"`
	Name              string    `json:"name"`
	Kind              string    `json:"kind"`
	Image             *string   `json:"image"`
	Compose           *string   `json:"compose"`
	Service           *string   `json:"service"`
	ContainerPort     *int      `json:"container_port"`
	HostPort          *int      `json:"host_port"`
	Domain            *string   `json:"domain"`
	DomainDisplay     string    `json:"domain_display"` // filled in by the API
	TLS               string    `json:"tls"`
	Env               []EnvVar  `json:"env"`
	Volumes           []string  `json:"volumes"`
	MemoryMB          *int      `json:"memory_mb"`
	CPUs              *float64  `json:"cpus"`
	PublicPorts       bool      `json:"public_ports"`
	Stopped           bool      `json:"stopped"`
	DeleteData        bool      `json:"-"`
	State             string    `json:"state"`
	Message           *string   `json:"message"`
	Generation        int64     `json:"generation"`
	AppliedGeneration int64     `json:"applied_generation"`
	OwnerID           *string   `json:"owner_id"`
	OwnerName         *string   `json:"owner_name"`
	Template          *string   `json:"template"` // catalogue template, if installed from it
	TemplateVersion   *int      `json:"template_version"`
	DiskBytes         *int64    `json:"disk_bytes"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

const appColumns = `a.id, a.server_id, sv.name, a.name, a.kind, a.image, a.compose, a.service, a.container_port,
	a.host_port, a.domain, a.tls, a.env, a.volumes, a.memory_mb, a.cpus::float8, a.public_ports, a.stopped,
	a.delete_data, a.state, a.message, a.generation, a.applied_generation, a.created_at, a.updated_at,
	a.owner_id, ow.name, a.disk_bytes, a.template, a.template_version`

const appFrom = ` FROM apps a JOIN servers sv ON sv.id = a.server_id LEFT JOIN users ow ON ow.id = a.owner_id`

func scanApp(row pgx.Row) (App, error) {
	var a App
	err := row.Scan(&a.ID, &a.ServerID, &a.ServerName, &a.Name, &a.Kind, &a.Image, &a.Compose, &a.Service, &a.ContainerPort,
		&a.HostPort, &a.Domain, &a.TLS, &a.Env, &a.Volumes, &a.MemoryMB, &a.CPUs, &a.PublicPorts, &a.Stopped,
		&a.DeleteData, &a.State, &a.Message, &a.Generation, &a.AppliedGeneration, &a.CreatedAt, &a.UpdatedAt,
		&a.OwnerID, &a.OwnerName, &a.DiskBytes, &a.Template, &a.TemplateVersion)
	if a.Env == nil {
		a.Env = []EnvVar{}
	}
	return a, notFound(err)
}

// ErrAppNameTaken means the server already has an app with that name.
var ErrAppNameTaken = errors.New("an app with this name already exists on the server")

// AppSettings is everything about an app that can change after it is created.
type AppSettings struct {
	Image         *string
	Compose       *string
	Service       *string
	ContainerPort *int
	Domain        *string
	TLS           string
	Env           []EnvVar
	Volumes       []string
	MemoryMB      *int
	CPUs          *float64
	PublicPorts   bool
}

type NewApp struct {
	ServerID  string
	Name      string
	Kind      string
	OwnerID   *string
	CreatedBy *string
	// Template and TemplateVersion name the catalogue template the app comes from.
	Template        *string
	TemplateVersion *int
	AppSettings
}

func claimAppHostname(ctx context.Context, tx pgx.Tx, appID string, domain *string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM site_hostnames WHERE app_id = $1`, appID); err != nil {
		return err
	}
	if domain == nil {
		return nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO site_hostnames (hostname, app_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, *domain, appID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrHostnameTaken{Hostname: *domain}
	}
	return nil
}

// hostPortSQL picks the lowest free port of the server's app range, keeping the app's own.
const hostPortSQL = `CASE WHEN $2::int IS NULL THEN NULL ELSE COALESCE(
	(SELECT host_port FROM apps WHERE id = $1 AND host_port IS NOT NULL),
	(SELECT p FROM generate_series(20000, 29999) p
	 WHERE p NOT IN (SELECT host_port FROM apps WHERE server_id = $3 AND host_port IS NOT NULL) ORDER BY p LIMIT 1)) END`

func (s *Store) CreateApp(ctx context.Context, n NewApp) (App, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return App{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&id)
	if err != nil {
		return App{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO apps (id, server_id, name, kind, image, compose, service, container_port, host_port, domain, tls,
			env, volumes, memory_mb, cpus, public_ports, created_by, owner_id, template, template_version)
		VALUES ($1, $3, $4, $5, $6, $7, $8, $2, `+hostPortSQL+`, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		id, n.ContainerPort, n.ServerID, n.Name, n.Kind, n.Image, n.Compose, n.Service, n.Domain, n.TLS,
		n.Env, n.Volumes, n.MemoryMB, n.CPUs, n.PublicPorts, n.CreatedBy, n.OwnerID, n.Template, n.TemplateVersion)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "apps_server_id_name_key" {
		return App{}, ErrAppNameTaken
	}
	if err != nil {
		return App{}, err
	}
	if err := claimAppHostname(ctx, tx, id, n.Domain); err != nil {
		return App{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return App{}, err
	}
	return s.GetApp(ctx, id)
}

func (s *Store) GetApp(ctx context.Context, id string) (App, error) {
	return scanApp(s.pool.QueryRow(ctx, `SELECT `+appColumns+appFrom+` WHERE a.id = $1`, id))
}

// ListApps returns apps on one server, or on all servers when serverID is empty.
func (s *Store) ListApps(ctx context.Context, serverID string) ([]App, error) {
	return s.ListAppsOf(ctx, serverID, "")
}

// ListAppsOf is ListApps for one owner only, unless ownerID is empty.
func (s *Store) ListAppsOf(ctx context.Context, serverID, ownerID string) ([]App, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+appColumns+appFrom+`
		WHERE ($1 = '' OR a.server_id::text = $1) AND ($2 = '' OR a.owner_id::text = $2) ORDER BY a.name, sv.name`, serverID, ownerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (App, error) { return scanApp(r) })
}

// UpdateApp replaces an app's settings and marks it for re-applying.
func (s *Store) UpdateApp(ctx context.Context, id string, c AppSettings) (App, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return App{}, err
	}
	defer tx.Rollback(ctx)
	var serverID string
	err = tx.QueryRow(ctx, `SELECT server_id FROM apps WHERE id = $1 AND state <> 'deleting' FOR UPDATE`, id).Scan(&serverID)
	if err != nil {
		return App{}, notFound(err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE apps SET image = $4, compose = $5, service = $6, container_port = $2, host_port = `+hostPortSQL+`,
			domain = $7, tls = $8, env = $9, volumes = $10, memory_mb = $11, cpus = $12, public_ports = $13,
			generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1`,
		id, c.ContainerPort, serverID, c.Image, c.Compose, c.Service, c.Domain, c.TLS, c.Env, c.Volumes,
		c.MemoryMB, c.CPUs, c.PublicPorts)
	if err != nil {
		return App{}, err
	}
	if err := claimAppHostname(ctx, tx, id, c.Domain); err != nil {
		return App{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return App{}, err
	}
	return s.GetApp(ctx, id)
}

// SetAppStopped starts or stops an app and marks it for re-applying.
func (s *Store) SetAppStopped(ctx context.Context, id string, stopped bool) (App, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE apps SET stopped = $2, generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1 AND state <> 'deleting'`, id, stopped)
	if err != nil {
		return App{}, err
	}
	if tag.RowsAffected() == 0 {
		return App{}, ErrNotFound
	}
	return s.GetApp(ctx, id)
}

// MarkAppDeleting asks the agent to remove the app; the row goes once it confirms.
func (s *Store) MarkAppDeleting(ctx context.Context, id string, deleteData bool) (App, error) {
	_, err := s.pool.Exec(ctx, `
		UPDATE apps SET state = 'deleting', delete_data = $2, message = NULL, generation = generation + 1, updated_at = now()
		WHERE id = $1`, id, deleteData)
	if err != nil {
		return App{}, err
	}
	return s.GetApp(ctx, id)
}

// ApplyAppResult records what the agent reported for one app at a given generation.
func (s *Store) ApplyAppResult(ctx context.Context, id string, generation int64, ok bool, message string) error {
	if ok {
		tag, err := s.pool.Exec(ctx, `DELETE FROM apps WHERE id = $1 AND state = 'deleting' AND generation = $2`, id, generation)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			UPDATE apps SET state = 'active', message = NULL, applied_generation = $2
			WHERE id = $1 AND generation = $2 AND state <> 'deleting'`, id, generation)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE apps SET message = $3, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE id = $1 AND generation = $2`, id, generation, message)
	return err
}

// FailUnappliedApps records an error on every app of a server waiting to be applied.
func (s *Store) FailUnappliedApps(ctx context.Context, serverID, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE apps SET message = $2, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE server_id = $1 AND generation > applied_generation`, serverID, message)
	return err
}
