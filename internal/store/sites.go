package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"ekpanel/internal/agentproto"
)

// ErrHostnameTaken means another site already answers to one of the requested host names.
type ErrHostnameTaken struct{ Hostname string }

func (e ErrHostnameTaken) Error() string {
	return e.Hostname + " is already used by another site or app"
}

type Site struct {
	ID         string   `json:"id"`
	ServerID   string   `json:"server_id"`
	ServerName string   `json:"server_name"`
	Domain     string   `json:"domain"`
	Aliases    []string `json:"aliases"`
	// Display forms (Unicode for internationalized names), filled in by the API.
	DomainDisplay     string             `json:"domain_display"`
	AliasesDisplay    []string           `json:"aliases_display"`
	Kind              string             `json:"kind"`
	PHPVersion        *string            `json:"php_version"`
	TLS               string             `json:"tls"`
	LinuxUser         string             `json:"linux_user"`
	SFTPEnabled       bool               `json:"sftp_enabled"`
	SSHKeys           []string           `json:"ssh_keys"`
	SFTPPasswordHash  *string            `json:"-"`
	FTPEnabled        bool               `json:"ftp_enabled"`
	FTPPasswordHash   *string            `json:"-"`
	App               *string            `json:"app"`
	AppVersion        *string            `json:"app_version"`
	State             string             `json:"state"`
	Message           *string            `json:"message"`
	Generation        int64              `json:"generation"`
	AppliedGeneration int64              `json:"applied_generation"`
	OwnerID           *string            `json:"owner_id"`
	OwnerName         *string            `json:"owner_name"`
	DiskBytes         *int64             `json:"disk_bytes"`
	Web               agentproto.SiteWeb `json:"web"`
	// Custom certificates: the chain, its expiry and names. The sealed key is not sent out.
	TLSCert      *string    `json:"-"`
	TLSKey       []byte     `json:"-"`
	TLSExpiresAt *time.Time `json:"tls_expires_at"`
	TLSNames     []string   `json:"tls_names"`
	// WordPress: the last scan, the updates ekPanel makes by itself, and the live site of a
	// staging copy.
	WordPress   *agentproto.WordPressInfo `json:"wordpress"`
	WPScannedAt *time.Time                `json:"wp_scanned_at"`
	WPAuto      WordPressAuto             `json:"wp_auto"`
	StagingOf   *string                   `json:"staging_of"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
}

// WordPressAuto is what ekPanel updates by itself: Core "off", "minor" or "all".
type WordPressAuto struct {
	Core    string `json:"core"`
	Plugins bool   `json:"plugins"`
	Themes  bool   `json:"themes"`
}

const siteColumns = `st.id, st.server_id, sv.name, st.domain, st.aliases, st.kind, st.php_version, st.tls,
	st.linux_user, st.state, st.message, st.generation, st.applied_generation, st.created_at, st.updated_at,
	st.sftp_enabled, st.ssh_keys, st.sftp_password_hash, st.app, st.app_version, st.owner_id, ow.name, st.disk_bytes,
	st.web, st.tls_cert, st.tls_key, st.tls_expires_at, st.tls_names, st.wp_info, st.wp_scanned_at, st.wp_auto, st.staging_of,
	st.ftp_enabled, st.ftp_password_hash`

const siteFrom = ` FROM sites st JOIN servers sv ON sv.id = st.server_id LEFT JOIN users ow ON ow.id = st.owner_id`

func scanSite(row pgx.Row) (Site, error) {
	var s Site
	err := row.Scan(&s.ID, &s.ServerID, &s.ServerName, &s.Domain, &s.Aliases, &s.Kind, &s.PHPVersion, &s.TLS,
		&s.LinuxUser, &s.State, &s.Message, &s.Generation, &s.AppliedGeneration, &s.CreatedAt, &s.UpdatedAt,
		&s.SFTPEnabled, &s.SSHKeys, &s.SFTPPasswordHash, &s.App, &s.AppVersion, &s.OwnerID, &s.OwnerName, &s.DiskBytes,
		&s.Web, &s.TLSCert, &s.TLSKey, &s.TLSExpiresAt, &s.TLSNames, &s.WordPress, &s.WPScannedAt, &s.WPAuto, &s.StagingOf,
		&s.FTPEnabled, &s.FTPPasswordHash)
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
	OwnerID    *string
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
		INSERT INTO sites (server_id, domain, aliases, kind, php_version, tls, linux_user, owner_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		n.ServerID, n.Domain, n.Aliases, n.Kind, n.PHPVersion, n.TLS, n.LinuxUser, n.OwnerID, n.CreatedBy).Scan(&id)
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
	return s.ListSitesOf(ctx, serverID, "")
}

// ListSitesOf is ListSites for one owner only, unless ownerID is empty.
func (s *Store) ListSitesOf(ctx context.Context, serverID, ownerID string) ([]Site, error) {
	return collectSites(s.pool.Query(ctx, `SELECT `+siteColumns+siteFrom+`
		WHERE ($1 = '' OR st.server_id::text = $1) AND ($2 = '' OR `+ownedBy("st.owner_id", "$2")+`) ORDER BY st.domain`,
		serverID, ownerID))
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
			tls_cert = CASE WHEN $5 = 'custom' THEN tls_cert END, tls_key = CASE WHEN $5 = 'custom' THEN tls_key END,
			tls_expires_at = CASE WHEN $5 = 'custom' THEN tls_expires_at END, tls_names = CASE WHEN $5 = 'custom' THEN tls_names END,
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

// SetSiteWeb replaces a site's redirects, hotlink protection and PHP settings.
func (s *Store) SetSiteWeb(ctx context.Context, id string, web agentproto.SiteWeb) (Site, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sites SET web = $2, generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1 AND state <> 'deleting'`, id, web)
	if err != nil {
		return Site{}, err
	}
	if tag.RowsAffected() == 0 {
		return Site{}, ErrNotFound
	}
	return s.GetSite(ctx, id)
}

// SiteCert is an uploaded certificate, checked by the caller.
type SiteCert struct {
	Chain     string
	SealedKey []byte
	ExpiresAt time.Time
	Names     []string
}

// SetSiteCert makes a site use an uploaded certificate, or (nil) a free one again.
func (s *Store) SetSiteCert(ctx context.Context, id string, c *SiteCert) (Site, error) {
	var err error
	if c != nil {
		_, err = s.pool.Exec(ctx, `
			UPDATE sites SET tls = 'custom', tls_cert = $2, tls_key = $3, tls_expires_at = $4, tls_names = $5,
				generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
			WHERE id = $1 AND state <> 'deleting'`, id, c.Chain, c.SealedKey, c.ExpiresAt, c.Names)
	} else {
		_, err = s.pool.Exec(ctx, `
			UPDATE sites SET tls = 'auto', tls_cert = NULL, tls_key = NULL, tls_expires_at = NULL, tls_names = NULL,
				generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
			WHERE id = $1 AND state <> 'deleting'`, id)
	}
	if err != nil {
		return Site{}, err
	}
	return s.GetSite(ctx, id)
}

// ExpiringCerts lists sites whose uploaded certificate expires before the given time.
func (s *Store) ExpiringCerts(ctx context.Context, before time.Time) ([]Site, error) {
	return collectSites(s.pool.Query(ctx, `SELECT `+siteColumns+siteFrom+`
		WHERE st.tls = 'custom' AND st.tls_expires_at < $1 AND st.state <> 'deleting'`, before))
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

// SetSiteFTP turns FTP on (with a new password hash) or off (forgetting the password).
func (s *Store) SetSiteFTP(ctx context.Context, id string, enabled bool, passwordHash *string) (Site, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sites SET ftp_enabled = $2,
			ftp_password_hash = CASE WHEN NOT $2 THEN NULL WHEN $3::text IS NOT NULL THEN $3 ELSE ftp_password_hash END,
			generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1 AND state <> 'deleting'`, id, enabled, passwordHash)
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

// ServersWithSites lists servers that host at least one site, database or app, or whose
// firewall ekPanel manages, or that have email or CrowdSec.
func (s *Store) ServersWithSites(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT server_id::text FROM sites UNION SELECT server_id::text FROM databases
		UNION SELECT server_id::text FROM apps UNION SELECT id::text FROM servers WHERE firewall <> 'unmanaged'
		UNION SELECT server_id::text FROM mail_servers UNION SELECT id::text FROM servers WHERE crowdsec`)
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

// HostnamesTaken returns which of the names a site or app already uses.
func (s *Store) HostnamesTaken(ctx context.Context, names []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT hostname FROM site_hostnames WHERE hostname = ANY($1) ORDER BY hostname`, names)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
