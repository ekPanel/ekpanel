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
	GitURL            *string   `json:"git_url"` // Git apps: where to build from
	GitBranch         *string   `json:"git_branch"`
	Dockerfile        *string   `json:"dockerfile"`
	BuildContext      *string   `json:"build_context"`
	AutoDeploy        bool      `json:"auto_deploy"`
	DeployKeyPublic   *string   `json:"deploy_key_public"`
	WebhookSecret     []byte    `json:"-"` // sealed
	DeployKey         []byte    `json:"-"` // sealed
	DiskBytes         *int64    `json:"disk_bytes"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

const appColumns = `a.id, a.server_id, sv.name, a.name, a.kind, a.image, a.compose, a.service, a.container_port,
	a.host_port, a.domain, a.tls, a.env, a.volumes, a.memory_mb, a.cpus::float8, a.public_ports, a.stopped,
	a.delete_data, a.state, a.message, a.generation, a.applied_generation, a.created_at, a.updated_at,
	a.owner_id, ow.name, a.disk_bytes, a.template, a.template_version, a.git_url, a.git_branch, a.dockerfile,
	a.build_context, a.auto_deploy, a.deploy_key_public, a.webhook_secret, a.deploy_key`

const appFrom = ` FROM apps a JOIN servers sv ON sv.id = a.server_id LEFT JOIN users ow ON ow.id = a.owner_id`

func scanApp(row pgx.Row) (App, error) {
	var a App
	err := row.Scan(&a.ID, &a.ServerID, &a.ServerName, &a.Name, &a.Kind, &a.Image, &a.Compose, &a.Service, &a.ContainerPort,
		&a.HostPort, &a.Domain, &a.TLS, &a.Env, &a.Volumes, &a.MemoryMB, &a.CPUs, &a.PublicPorts, &a.Stopped,
		&a.DeleteData, &a.State, &a.Message, &a.Generation, &a.AppliedGeneration, &a.CreatedAt, &a.UpdatedAt,
		&a.OwnerID, &a.OwnerName, &a.DiskBytes, &a.Template, &a.TemplateVersion, &a.GitURL, &a.GitBranch, &a.Dockerfile,
		&a.BuildContext, &a.AutoDeploy, &a.DeployKeyPublic, &a.WebhookSecret, &a.DeployKey)
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
	// Git apps.
	GitURL       *string
	GitBranch    *string
	Dockerfile   *string
	BuildContext *string
	AutoDeploy   bool
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
	// Git apps: sealed secrets made when the app is created.
	WebhookSecret   []byte
	DeployKey       []byte
	DeployKeyPublic *string
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
// Git apps ($4 = 'git') take two consecutive ports, host_port and host_port + 1.
const hostPortSQL = `CASE WHEN $2::int IS NULL THEN NULL ELSE COALESCE(
	(SELECT host_port FROM apps WHERE id = $1 AND host_port IS NOT NULL AND (kind = 'git') = ($4 = 'git')),
	(SELECT p FROM generate_series(20000, 29998) p
	 WHERE p NOT IN (SELECT u FROM apps a, LATERAL (VALUES (a.host_port), (CASE WHEN a.kind = 'git' THEN a.host_port + 1 END)) v(u)
	                 WHERE a.server_id = $3 AND a.id <> $1 AND u IS NOT NULL)
	   AND ($4 <> 'git' OR p + 1 NOT IN (SELECT u FROM apps a, LATERAL (VALUES (a.host_port), (CASE WHEN a.kind = 'git' THEN a.host_port + 1 END)) v(u)
	                 WHERE a.server_id = $3 AND a.id <> $1 AND u IS NOT NULL))
	 ORDER BY p LIMIT 1)) END`

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
		INSERT INTO apps (id, server_id, kind, name, image, compose, service, container_port, host_port, domain, tls,
			env, volumes, memory_mb, cpus, public_ports, created_by, owner_id, template, template_version,
			git_url, git_branch, dockerfile, build_context, auto_deploy, webhook_secret, deploy_key, deploy_key_public)
		VALUES ($1, $3, $4, $5, $6, $7, $8, $2, `+hostPortSQL+`, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19,
			$20, $21, $22, $23, $24, $25, $26, $27)`,
		id, n.ContainerPort, n.ServerID, n.Kind, n.Name, n.Image, n.Compose, n.Service, n.Domain, n.TLS,
		n.Env, n.Volumes, n.MemoryMB, n.CPUs, n.PublicPorts, n.CreatedBy, n.OwnerID, n.Template, n.TemplateVersion,
		n.GitURL, n.GitBranch, n.Dockerfile, n.BuildContext, n.AutoDeploy, n.WebhookSecret, n.DeployKey, n.DeployKeyPublic)
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
	var serverID, kind string
	err = tx.QueryRow(ctx, `SELECT server_id, kind FROM apps WHERE id = $1 AND state <> 'deleting' FOR UPDATE`, id).Scan(&serverID, &kind)
	if err != nil {
		return App{}, notFound(err)
	}
	// A Git app's image is the live build; it changes only by deploying.
	_, err = tx.Exec(ctx, `
		UPDATE apps SET image = CASE WHEN kind = 'git' THEN image ELSE $5 END, compose = $6, service = $7, container_port = $2,
			host_port = `+hostPortSQL+`, domain = $8, tls = $9, env = $10, volumes = $11, memory_mb = $12, cpus = $13,
			public_ports = $14, git_url = $15, git_branch = $16, dockerfile = $17, build_context = $18, auto_deploy = $19,
			generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1`,
		id, c.ContainerPort, serverID, kind, c.Image, c.Compose, c.Service, c.Domain, c.TLS, c.Env, c.Volumes,
		c.MemoryMB, c.CPUs, c.PublicPorts, c.GitURL, c.GitBranch, c.Dockerfile, c.BuildContext, c.AutoDeploy)
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

// SetAppImage makes image the live build of a Git app (nil: none) and marks the app for
// re-applying, which releases it. It returns the new generation.
func (s *Store) SetAppImage(ctx context.Context, id string, image *string) (int64, error) {
	var gen int64
	err := s.pool.QueryRow(ctx, `
		UPDATE apps SET image = $2, generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1 AND kind = 'git' AND state <> 'deleting' RETURNING generation`, id, image).Scan(&gen)
	return gen, notFound(err)
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
