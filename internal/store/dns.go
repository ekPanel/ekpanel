package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// CloudflareAccount is a connected Cloudflare account. The sealed token never leaves.
type CloudflareAccount struct {
	ID          string    `json:"id"`
	OwnerID     *string   `json:"owner_id"`
	Name        string    `json:"name"`
	Token       []byte    `json:"-"`
	AccountID   *string   `json:"account_id"`
	AccountName *string   `json:"account_name"`
	Zones       int       `json:"zones"`
	CreatedAt   time.Time `json:"created_at"`
}

const cfAccountColumns = `c.id, c.owner_id, c.name, c.token, c.account_id, c.account_name, c.created_at,
	(SELECT count(*) FROM dns_zones z WHERE z.account_id = c.id)`

func scanCFAccount(row pgx.Row) (CloudflareAccount, error) {
	var a CloudflareAccount
	err := row.Scan(&a.ID, &a.OwnerID, &a.Name, &a.Token, &a.AccountID, &a.AccountName, &a.CreatedAt, &a.Zones)
	return a, notFound(err)
}

func (s *Store) CreateCloudflareAccount(ctx context.Context, a CloudflareAccount, createdBy *string) (CloudflareAccount, error) {
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO cloudflare_accounts (owner_id, name, token, account_id, account_name, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, a.OwnerID, a.Name, a.Token, a.AccountID, a.AccountName, createdBy).Scan(&id)
	if err != nil {
		return a, err
	}
	return s.GetCloudflareAccount(ctx, id)
}

func (s *Store) GetCloudflareAccount(ctx context.Context, id string) (CloudflareAccount, error) {
	return scanCFAccount(s.pool.QueryRow(ctx, `SELECT `+cfAccountColumns+` FROM cloudflare_accounts c WHERE c.id = $1`, id))
}

// ListCloudflareAccounts lists the accounts of one owner ("" for administrators').
func (s *Store) ListCloudflareAccounts(ctx context.Context, ownerID string) ([]CloudflareAccount, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cfAccountColumns+` FROM cloudflare_accounts c
		WHERE ($1 = '' AND c.owner_id IS NULL) OR ($1 <> '' AND `+ownedBy("c.owner_id", "$1")+`) ORDER BY lower(c.name)`, ownerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CloudflareAccount, error) { return scanCFAccount(r) })
}

func (s *Store) DeleteCloudflareAccount(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM cloudflare_accounts WHERE id = $1`, id)
	return err
}

// DNSZone is a Cloudflare zone ekPanel keeps in sync.
type DNSZone struct {
	ID          string          `json:"id"`
	AccountID   string          `json:"account_id"`
	AccountName string          `json:"account_name"`
	OwnerID     *string         `json:"owner_id"`
	CFZoneID    string          `json:"cf_zone_id"`
	Name        string          `json:"name"`
	Status      string          `json:"status"`
	NameServers []string        `json:"name_servers"`
	Proxied     bool            `json:"proxied"`
	Issues      json.RawMessage `json:"issues"`
	SyncError   *string         `json:"sync_error"`
	SyncedAt    *time.Time      `json:"synced_at"`
	Records     int             `json:"records"` // made by ekPanel
	CreatedAt   time.Time       `json:"created_at"`
}

const zoneColumns = `z.id, z.account_id, c.name, z.owner_id, z.cf_zone_id, z.name, z.status, z.name_servers, z.proxied, z.issues,
	z.sync_error, z.synced_at, z.created_at, (SELECT count(*) FROM dns_records r WHERE r.zone_id = z.id)`

const zoneFrom = ` FROM dns_zones z JOIN cloudflare_accounts c ON c.id = z.account_id`

func scanZone(row pgx.Row) (DNSZone, error) {
	var z DNSZone
	err := row.Scan(&z.ID, &z.AccountID, &z.AccountName, &z.OwnerID, &z.CFZoneID, &z.Name, &z.Status, &z.NameServers, &z.Proxied, &z.Issues,
		&z.SyncError, &z.SyncedAt, &z.CreatedAt, &z.Records)
	return z, notFound(err)
}

// ErrZoneTaken means ekPanel already manages a zone of that name.
var ErrZoneTaken = errorString("this domain is already managed in ekPanel")

type errorString string

func (e errorString) Error() string { return string(e) }

func (s *Store) CreateZone(ctx context.Context, z DNSZone) (DNSZone, error) {
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO dns_zones (account_id, owner_id, cf_zone_id, name, status, name_servers, proxied)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`, z.AccountID, z.OwnerID, z.CFZoneID, z.Name, z.Status, z.NameServers, z.Proxied).Scan(&id)
	if isUniqueViolation(err) {
		return z, ErrZoneTaken
	}
	if err != nil {
		return z, err
	}
	return s.GetZone(ctx, id)
}

func (s *Store) GetZone(ctx context.Context, id string) (DNSZone, error) {
	return scanZone(s.pool.QueryRow(ctx, `SELECT `+zoneColumns+zoneFrom+` WHERE z.id = $1`, id))
}

// ListZones lists zones, all of them or one owner's ("" for administrators' when owned).
func (s *Store) ListZones(ctx context.Context, all bool, ownerID string) ([]DNSZone, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+zoneColumns+zoneFrom+`
		WHERE $1 OR ($2 = '' AND z.owner_id IS NULL) OR ($2 <> '' AND `+ownedBy("z.owner_id", "$2")+`) ORDER BY z.name`, all, ownerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DNSZone, error) { return scanZone(r) })
}

// HasZones reports whether any DNS zone is connected or hosted.
func (s *Store) HasZones(ctx context.Context) (bool, error) {
	var has bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dns_zones) OR EXISTS (SELECT 1 FROM hosted_zones)`).Scan(&has)
	return has, err
}

func (s *Store) SetZoneProxied(ctx context.Context, id string, proxied bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE dns_zones SET proxied = $2 WHERE id = $1`, id, proxied)
	return err
}

// ZoneSynced records the result of a sync.
func (s *Store) ZoneSynced(ctx context.Context, id, status string, nameServers []string, issues any, syncErr *string) error {
	b, _ := json.Marshal(issues)
	if string(b) == "null" {
		b = []byte("[]")
	}
	_, err := s.pool.Exec(ctx, `UPDATE dns_zones SET status = $2, name_servers = $3, issues = $4, sync_error = $5, synced_at = now()
		WHERE id = $1`, id, status, nameServers, b, syncErr)
	return err
}

func (s *Store) DeleteZone(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM dns_zones WHERE id = $1`, id)
	return err
}

// DNSRecord is a record ekPanel made in a zone.
type DNSRecord struct {
	CFRecordID string `json:"cf_record_id"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	Proxied    bool   `json:"proxied"`
}

func (s *Store) ManagedRecords(ctx context.Context, zoneID string) ([]DNSRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT cf_record_id, type, name, content, proxied FROM dns_records WHERE zone_id = $1 ORDER BY name, type`, zoneID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DNSRecord])
}

// KeepRecord adds or replaces a managed record (one per type and name).
func (s *Store) KeepRecord(ctx context.Context, zoneID string, r DNSRecord) error {
	_, err := s.pool.Exec(ctx, `
		WITH gone AS (DELETE FROM dns_records WHERE zone_id = $1 AND type = $3 AND name = $4 AND cf_record_id <> $2)
		INSERT INTO dns_records (zone_id, cf_record_id, type, name, content, proxied) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (zone_id, cf_record_id) DO UPDATE SET type = $3, name = $4, content = $5, proxied = $6`,
		zoneID, r.CFRecordID, r.Type, r.Name, r.Content, r.Proxied)
	return err
}

func (s *Store) ForgetRecord(ctx context.Context, zoneID, recordID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM dns_records WHERE zone_id = $1 AND cf_record_id = $2`, zoneID, recordID)
	return err
}

// HostTarget is a host name of a website or app and the server it is on.
type HostTarget struct {
	Hostname   string
	OwnerID    *string
	ServerName string
	PublicIPv4 *string
	PublicIPv6 *string
	Addresses  []string
}

// HostTargets lists every host name of websites and apps that are not being deleted.
func (s *Store) HostTargets(ctx context.Context) ([]HostTarget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT h.hostname, COALESCE(st.owner_id, a.owner_id), sv.name, host(sv.public_ipv4), host(sv.public_ipv6), sv.addresses
		FROM site_hostnames h
		LEFT JOIN sites st ON st.id = h.site_id
		LEFT JOIN apps a ON a.id = h.app_id
		JOIN servers sv ON sv.id = COALESCE(st.server_id, a.server_id)
		WHERE COALESCE(st.state, a.state) <> 'deleting'
		ORDER BY h.hostname`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[HostTarget])
}
