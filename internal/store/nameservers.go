package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"ekpanel/internal/agentproto"
)

// NameServer is an ekPanel server that answers DNS for hosted zones.
type NameServer struct {
	ServerID        string                       `json:"server_id"`
	ServerName      string                       `json:"server_name"`
	Hostname        string                       `json:"hostname"`
	Primary         bool                         `json:"primary"`
	TransferAddress *string                      `json:"transfer_address"`
	Status          *agentproto.NameServerResult `json:"status"`
	CheckedAt       *time.Time                   `json:"checked_at"`
	// The server's addresses, for glue records and zone transfers.
	PublicIPv4 *string  `json:"public_ipv4"`
	PublicIPv6 *string  `json:"public_ipv6"`
	Addresses  []string `json:"addresses"`
}

// Target is the server as a host target, for picking its public addresses.
func (n NameServer) Target() HostTarget {
	return HostTarget{Hostname: n.Hostname, ServerName: n.ServerName, PublicIPv4: n.PublicIPv4, PublicIPv6: n.PublicIPv6, Addresses: n.Addresses}
}

func (s *Store) ListNameServers(ctx context.Context) ([]NameServer, error) {
	rows, err := s.pool.Query(ctx, `SELECT n.server_id, sv.name, n.hostname, n.is_primary, host(n.transfer_address), n.status, n.checked_at,
		host(sv.public_ipv4), host(sv.public_ipv6), sv.addresses
		FROM name_servers n JOIN servers sv ON sv.id = n.server_id ORDER BY n.position, n.hostname`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (NameServer, error) {
		var n NameServer
		err := r.Scan(&n.ServerID, &n.ServerName, &n.Hostname, &n.Primary, &n.TransferAddress, &n.Status, &n.CheckedAt,
			&n.PublicIPv4, &n.PublicIPv6, &n.Addresses)
		return n, err
	})
}

// SetNameServers replaces the list of name servers, keeping what servers reported.
func (s *Store) SetNameServers(ctx context.Context, list []NameServer) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	ids := make([]string, len(list))
	for i, n := range list {
		ids[i] = n.ServerID
	}
	if _, err := tx.Exec(ctx, `DELETE FROM name_servers WHERE NOT (server_id::text = ANY($1))`, ids); err != nil {
		return err
	}
	// Clear the primary first: only one may be marked at a time.
	if _, err := tx.Exec(ctx, `UPDATE name_servers SET is_primary = false, hostname = 'tmp-' || server_id`); err != nil {
		return err
	}
	for i, n := range list {
		if _, err := tx.Exec(ctx, `INSERT INTO name_servers (server_id, hostname, is_primary, transfer_address, position)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (server_id) DO UPDATE SET hostname = $2, is_primary = $3, transfer_address = $4, position = $5`,
			n.ServerID, n.Hostname, n.Primary, n.TransferAddress, i); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) RecordNameServerStatus(ctx context.Context, serverID string, res agentproto.NameServerResult) error {
	_, err := s.pool.Exec(ctx, `UPDATE name_servers SET status = $2, checked_at = now() WHERE server_id = $1`, serverID, res)
	return err
}

// HostedZone is a zone on ekPanel's own name servers.
type HostedZone struct {
	ID        string          `json:"id"`
	OwnerID   *string         `json:"owner_id"`
	OwnerName *string         `json:"owner_name"`
	Name      string          `json:"name"`
	DNSSEC    bool            `json:"dnssec"`
	Serial    int64           `json:"serial"`
	DS        []string        `json:"ds"`
	Error     *string         `json:"error"`
	Issues    json.RawMessage `json:"issues"`
	Records   int             `json:"records"` // kept by people
	CreatedAt time.Time       `json:"created_at"`
}

const hostedZoneColumns = `z.id, z.owner_id, u.name, z.name, z.dnssec, z.serial, z.ds, z.error, z.issues,
	(SELECT count(*) FROM hosted_records r WHERE r.zone_id = z.id), z.created_at`

const hostedZoneFrom = ` FROM hosted_zones z LEFT JOIN users u ON u.id = z.owner_id`

func scanHostedZone(row pgx.Row) (HostedZone, error) {
	var z HostedZone
	err := row.Scan(&z.ID, &z.OwnerID, &z.OwnerName, &z.Name, &z.DNSSEC, &z.Serial, &z.DS, &z.Error, &z.Issues, &z.Records, &z.CreatedAt)
	return z, notFound(err)
}

// HostedRecord is a record kept in a hosted zone.
type HostedRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	Priority int    `json:"priority"`
	TTL      int    `json:"ttl"`
}

func (r HostedRecord) Spec() agentproto.HostedRecord {
	return agentproto.HostedRecord{Name: r.Name, Type: r.Type, Content: r.Content, Priority: r.Priority, TTL: r.TTL}
}

// ErrRecordExists: the zone already has the same record.
var ErrRecordExists = errors.New("this record already exists")

// CreateHostedZone adds a zone with its first records.
func (s *Store) CreateHostedZone(ctx context.Context, name string, ownerID, createdBy *string, records []agentproto.HostedRecord) (HostedZone, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return HostedZone{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO hosted_zones (name, owner_id, created_by) VALUES ($1, $2, $3) RETURNING id`, name, ownerID, createdBy).Scan(&id)
	if isUniqueViolation(err) {
		return HostedZone{}, ErrZoneTaken
	}
	if err != nil {
		return HostedZone{}, err
	}
	for _, r := range records {
		if _, err := tx.Exec(ctx, `INSERT INTO hosted_records (zone_id, name, type, content, priority, ttl) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING`, id, r.Name, r.Type, r.Content, r.Priority, r.TTL); err != nil {
			return HostedZone{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return HostedZone{}, err
	}
	return s.GetHostedZone(ctx, id)
}

func (s *Store) GetHostedZone(ctx context.Context, id string) (HostedZone, error) {
	return scanHostedZone(s.pool.QueryRow(ctx, `SELECT `+hostedZoneColumns+hostedZoneFrom+` WHERE z.id::text = $1`, id))
}

// ListHostedZones lists every zone (all), or one owner's ("" for the administrators').
func (s *Store) ListHostedZones(ctx context.Context, all bool, ownerID string) ([]HostedZone, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+hostedZoneColumns+hostedZoneFrom+`
		WHERE $1 OR ($2 = '' AND z.owner_id IS NULL) OR ($2 <> '' AND `+ownedBy("z.owner_id", "$2")+`) ORDER BY z.name`, all, ownerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (HostedZone, error) { return scanHostedZone(r) })
}

func (s *Store) DeleteHostedZone(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM hosted_zones WHERE id = $1`, id)
	return err
}

func (s *Store) SetHostedZoneDNSSEC(ctx context.Context, id string, on bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE hosted_zones SET dnssec = $2, ds = CASE WHEN $2 THEN ds ELSE '{}' END WHERE id = $1`, id, on)
	return err
}

// HostedZoneApplied records what the primary reported for a zone.
func (s *Store) HostedZoneApplied(ctx context.Context, name string, serial int64, ds []string, applyErr *string) error {
	if ds == nil {
		ds = []string{}
	}
	_, err := s.pool.Exec(ctx, `UPDATE hosted_zones SET serial = $2, ds = $3, error = $4 WHERE name = $1`, name, serial, ds, applyErr)
	return err
}

// SetHostedZoneIssues keeps what ekPanel could not add to a zone.
func (s *Store) SetHostedZoneIssues(ctx context.Context, id string, issues any) error {
	b, _ := json.Marshal(issues)
	if string(b) == "null" {
		b = []byte("[]")
	}
	_, err := s.pool.Exec(ctx, `UPDATE hosted_zones SET issues = $2 WHERE id = $1 AND issues <> $2::jsonb`, id, b)
	return err
}

func (s *Store) ListHostedRecords(ctx context.Context, zoneID string) ([]HostedRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, type, content, priority, ttl FROM hosted_records WHERE zone_id = $1
		ORDER BY name = (SELECT name FROM hosted_zones WHERE id = $1) DESC, name, type, priority, content`, zoneID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[HostedRecord])
}

// AllHostedRecords lists the records of every zone, by zone ID.
func (s *Store) AllHostedRecords(ctx context.Context) (map[string][]HostedRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT zone_id, id, name, type, content, priority, ttl FROM hosted_records ORDER BY name, type, priority, content`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]HostedRecord{}
	for rows.Next() {
		var zone string
		var r HostedRecord
		if err := rows.Scan(&zone, &r.ID, &r.Name, &r.Type, &r.Content, &r.Priority, &r.TTL); err != nil {
			return nil, err
		}
		out[zone] = append(out[zone], r)
	}
	return out, rows.Err()
}

func (s *Store) AddHostedRecord(ctx context.Context, zoneID string, r agentproto.HostedRecord) (HostedRecord, error) {
	var out HostedRecord
	err := s.pool.QueryRow(ctx, `INSERT INTO hosted_records (zone_id, name, type, content, priority, ttl) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, name, type, content, priority, ttl`, zoneID, r.Name, r.Type, r.Content, r.Priority, r.TTL).
		Scan(&out.ID, &out.Name, &out.Type, &out.Content, &out.Priority, &out.TTL)
	if isUniqueViolation(err) {
		return out, ErrRecordExists
	}
	return out, err
}

func (s *Store) UpdateHostedRecord(ctx context.Context, zoneID, id string, r agentproto.HostedRecord) error {
	tag, err := s.pool.Exec(ctx, `UPDATE hosted_records SET name = $3, type = $4, content = $5, priority = $6, ttl = $7
		WHERE zone_id = $1 AND id::text = $2`, zoneID, id, r.Name, r.Type, r.Content, r.Priority, r.TTL)
	if isUniqueViolation(err) {
		return ErrRecordExists
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) DeleteHostedRecord(ctx context.Context, zoneID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM hosted_records WHERE zone_id = $1 AND id::text = $2`, zoneID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ZoneNameTaken reports whether a zone of that name exists, on Cloudflare or hosted.
func (s *Store) ZoneNameTaken(ctx context.Context, name string) (bool, error) {
	var taken bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dns_zones WHERE name = $1) OR EXISTS (SELECT 1 FROM hosted_zones WHERE name = $1)`, name).Scan(&taken)
	return taken, err
}

// DNSTemplate is a set of records for new zones: names relative to the zone ("@" for the
// zone itself), and "{domain}" in contents standing for the zone's name.
type DNSTemplate struct {
	ID        string                    `json:"id"`
	Name      string                    `json:"name"`
	Records   []agentproto.HostedRecord `json:"records"`
	CreatedAt time.Time                 `json:"created_at"`
}

func (s *Store) ListDNSTemplates(ctx context.Context) ([]DNSTemplate, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, records, created_at FROM dns_templates ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DNSTemplate])
}

func (s *Store) GetDNSTemplate(ctx context.Context, id string) (DNSTemplate, error) {
	var t DNSTemplate
	err := s.pool.QueryRow(ctx, `SELECT id, name, records, created_at FROM dns_templates WHERE id::text = $1`, id).Scan(&t.ID, &t.Name, &t.Records, &t.CreatedAt)
	return t, notFound(err)
}

// ErrTemplateNameTaken: another template has that name.
var ErrTemplateNameTaken = errors.New("a template with this name already exists")

// SaveDNSTemplate adds a template (no ID) or replaces one.
func (s *Store) SaveDNSTemplate(ctx context.Context, t DNSTemplate) (DNSTemplate, error) {
	if t.Records == nil {
		t.Records = []agentproto.HostedRecord{}
	}
	var err error
	if t.ID == "" {
		err = s.pool.QueryRow(ctx, `INSERT INTO dns_templates (name, records) VALUES ($1, $2) RETURNING id`, t.Name, t.Records).Scan(&t.ID)
	} else {
		var tag interface{ RowsAffected() int64 }
		tag, err = s.pool.Exec(ctx, `UPDATE dns_templates SET name = $2, records = $3 WHERE id::text = $1`, t.ID, t.Name, t.Records)
		if err == nil && tag.RowsAffected() == 0 {
			return t, ErrNotFound
		}
	}
	if isUniqueViolation(err) {
		return t, ErrTemplateNameTaken
	}
	if err != nil {
		return t, err
	}
	return s.GetDNSTemplate(ctx, t.ID)
}

func (s *Store) DeleteDNSTemplate(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM dns_templates WHERE id::text = $1`, id)
	return err
}

// OwnerHostnames lists the host names of an owner's websites and apps.
func (s *Store) OwnerHostnames(ctx context.Context, ownerID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT h.hostname FROM site_hostnames h
		LEFT JOIN sites st ON st.id = h.site_id LEFT JOIN apps a ON a.id = h.app_id
		WHERE COALESCE(st.owner_id, a.owner_id)::text = $1`, ownerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
