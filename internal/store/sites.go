package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrHostnameTaken means another site already answers to one of the requested host names.
type ErrHostnameTaken struct{ Hostname string }

func (e ErrHostnameTaken) Error() string { return e.Hostname + " is already used by another site" }

type Site struct {
	ID         string   `json:"id"`
	ServerID   string   `json:"server_id"`
	ServerName string   `json:"server_name"`
	Domain     string   `json:"domain"`
	Aliases    []string `json:"aliases"`
	// Display forms (Unicode for internationalized names), filled in by the API.
	DomainDisplay     string    `json:"domain_display"`
	AliasesDisplay    []string  `json:"aliases_display"`
	Kind              string    `json:"kind"`
	PHPVersion        *string   `json:"php_version"`
	TLS               string    `json:"tls"`
	LinuxUser         string    `json:"linux_user"`
	SFTPEnabled       bool      `json:"sftp_enabled"`
	SSHKeys           []string  `json:"ssh_keys"`
	SFTPPasswordHash  *string   `json:"-"`
	App               *string   `json:"app"`
	AppVersion        *string   `json:"app_version"`
	State             string    `json:"state"`
	Message           *string   `json:"message"`
	Generation        int64     `json:"generation"`
	AppliedGeneration int64     `json:"applied_generation"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

const siteColumns = `st.id, st.server_id, sv.name, st.domain, st.aliases, st.kind, st.php_version, st.tls,
	st.linux_user, st.state, st.message, st.generation, st.applied_generation, st.created_at, st.updated_at,
	st.sftp_enabled, st.ssh_keys, st.sftp_password_hash, st.app, st.app_version`

const siteFrom = ` FROM sites st JOIN servers sv ON sv.id = st.server_id`

func scanSite(row pgx.Row) (Site, error) {
	var s Site
	err := row.Scan(&s.ID, &s.ServerID, &s.ServerName, &s.Domain, &s.Aliases, &s.Kind, &s.PHPVersion, &s.TLS,
		&s.LinuxUser, &s.State, &s.Message, &s.Generation, &s.AppliedGeneration, &s.CreatedAt, &s.UpdatedAt,
		&s.SFTPEnabled, &s.SSHKeys, &s.SFTPPasswordHash, &s.App, &s.AppVersion)
	return s, notFound(err)
}

func collectSites(rows pgx.Rows, err error) ([]Site, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Site, error) { return scanSite(r) })
}

type NewSite struct {
	ServerID   string
	Domain     string
	Aliases    []string
	Kind       string
	PHPVersion *string
	TLS        string
	LinuxUser  string
	CreatedBy  *string
}

// claimHostnames records every host name of a site, failing with ErrHostnameTaken if one
// already belongs to another site.
func claimHostnames(ctx context.Context, tx pgx.Tx, siteID string, hostnames []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM site_hostnames WHERE site_id = $1`, siteID); err != nil {
		return err
	}
	for _, h := range hostnames {
		tag, err := tx.Exec(ctx, `INSERT INTO site_hostnames (hostname, site_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, h, siteID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrHostnameTaken{Hostname: h}
		}
	}
	return nil
}

func (s *Store) CreateSite(ctx context.Context, n NewSite) (Site, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Site{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO sites (server_id, domain, aliases, kind, php_version, tls, linux_user, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		n.ServerID, n.Domain, n.Aliases, n.Kind, n.PHPVersion, n.TLS, n.LinuxUser, n.CreatedBy).Scan(&id)
	if err != nil {
		return Site{}, err
	}
	if err := claimHostnames(ctx, tx, id, append([]string{n.Domain}, n.Aliases...)); err != nil {
		return Site{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Site{}, err
	}
	return s.GetSite(ctx, id)
}

func (s *Store) GetSite(ctx context.Context, id string) (Site, error) {
	return scanSite(s.pool.QueryRow(ctx, `SELECT `+siteColumns+siteFrom+` WHERE st.id = $1`, id))
}

// ListSites returns sites on one server, or on all servers when serverID is empty.
func (s *Store) ListSites(ctx context.Context, serverID string) ([]Site, error) {
	return collectSites(s.pool.Query(ctx, `SELECT `+siteColumns+siteFrom+`
		WHERE ($1 = '' OR st.server_id::text = $1) ORDER BY st.domain`, serverID))
}

type SiteChange struct {
	Aliases    []string
	Kind       string
	PHPVersion *string
	TLS        string
}

// UpdateSite replaces a site's settings and marks it for re-applying.
func (s *Store) UpdateSite(ctx context.Context, id string, c SiteChange) (Site, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Site{}, err
	}
	defer tx.Rollback(ctx)
	var domain string
	err = tx.QueryRow(ctx, `
		UPDATE sites SET aliases = $2, kind = $3, php_version = $4, tls = $5,
			generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1 AND state <> 'deleting' RETURNING domain`, id, c.Aliases, c.Kind, c.PHPVersion, c.TLS).Scan(&domain)
	if err != nil {
		return Site{}, notFound(err)
	}
	if err := claimHostnames(ctx, tx, id, append([]string{domain}, c.Aliases...)); err != nil {
		return Site{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Site{}, err
	}
	return s.GetSite(ctx, id)
}

// SetSiteSFTP changes SFTP access and marks the site for re-applying. passwordHash nil
// keeps the current one; keys nil keeps the current keys.
func (s *Store) SetSiteSFTP(ctx context.Context, id string, enabled bool, passwordHash *string, clearPassword bool, keys []string) (Site, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sites SET sftp_enabled = $2,
			sftp_password_hash = CASE WHEN $4 THEN NULL WHEN $3::text IS NOT NULL THEN $3 ELSE sftp_password_hash END,
			ssh_keys = COALESCE($5, ssh_keys),
			generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1 AND state <> 'deleting'`, id, enabled, passwordHash, clearPassword, keys)
	if err != nil {
		return Site{}, err
	}
	if tag.RowsAffected() == 0 {
		return Site{}, ErrNotFound
	}
	return s.GetSite(ctx, id)
}

// SetSiteApp records the application installed on a site.
func (s *Store) SetSiteApp(ctx context.Context, id, app, version string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sites SET app = $2, app_version = $3, updated_at = now() WHERE id = $1`, id, app, version)
	return err
}

// MarkSiteDeleting asks the agent to remove the site; the row goes once it confirms.
func (s *Store) MarkSiteDeleting(ctx context.Context, id string) (Site, error) {
	_, err := s.pool.Exec(ctx, `
		UPDATE sites SET state = 'deleting', message = NULL, generation = generation + 1, updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return Site{}, err
	}
	return s.GetSite(ctx, id)
}

// ApplySiteResult records what the agent reported for one site at a given generation.
// A result for an older generation (the site changed meanwhile) only records errors.
func (s *Store) ApplySiteResult(ctx context.Context, id string, generation int64, ok bool, message string) error {
	if ok {
		tag, err := s.pool.Exec(ctx, `DELETE FROM sites WHERE id = $1 AND state = 'deleting' AND generation = $2`, id, generation)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			UPDATE sites SET state = 'active', message = NULL, applied_generation = $2
			WHERE id = $1 AND generation = $2 AND state <> 'deleting'`, id, generation)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sites SET message = $3, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE id = $1 AND generation = $2`, id, generation, message)
	return err
}

// FailUnappliedSites records an error on every site of a server that is waiting to be
// applied, when the agent could not run the apply at all.
func (s *Store) FailUnappliedSites(ctx context.Context, serverID, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sites SET message = $2, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE server_id = $1 AND generation > applied_generation`, serverID, message)
	return err
}

// ServersWithSites lists servers that host at least one site or database.
func (s *Store) ServersWithSites(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT server_id::text FROM sites UNION SELECT server_id::text FROM databases`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// LinuxUserTaken reports whether a Linux account name is already assigned to a site.
func (s *Store) LinuxUserTaken(ctx context.Context, name string) (bool, error) {
	var taken bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sites WHERE linux_user = $1)`, name).Scan(&taken)
	return taken, err
}
