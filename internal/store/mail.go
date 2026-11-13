package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"ekpanel/internal/agentproto"
)

var (
	// ErrMailHostTaken means another server already uses that mail host name.
	ErrMailHostTaken = errors.New("another server already uses this mail host name")
	// ErrMailDomainTaken means the domain already has email in the panel.
	ErrMailDomainTaken = errors.New("this domain already has email")
	// ErrMailAddressTaken means the domain already has a mailbox or forwarder of that name.
	ErrMailAddressTaken = errors.New("this address is already used")
)

// ---- Mail servers ----

type MailServer struct {
	ServerID   string    `json:"server_id"`
	ServerName string    `json:"server_name"`
	Hostname   string    `json:"hostname"`
	TLS        string    `json:"tls"`
	State      string    `json:"state"`
	Message    *string   `json:"message"`
	Domains    int       `json:"domains"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const mailServerColumns = `m.server_id, sv.name, m.hostname, m.tls, m.state, m.message,
	(SELECT count(*) FROM mail_domains d WHERE d.server_id = m.server_id), m.created_at, m.updated_at`

func scanMailServer(row pgx.Row) (MailServer, error) {
	var m MailServer
	err := row.Scan(&m.ServerID, &m.ServerName, &m.Hostname, &m.TLS, &m.State, &m.Message, &m.Domains, &m.CreatedAt, &m.UpdatedAt)
	return m, notFound(err)
}

func (s *Store) GetMailServer(ctx context.Context, serverID string) (MailServer, error) {
	return scanMailServer(s.pool.QueryRow(ctx, `SELECT `+mailServerColumns+`
		FROM mail_servers m JOIN servers sv ON sv.id = m.server_id WHERE m.server_id = $1`, serverID))
}

func (s *Store) ListMailServers(ctx context.Context) ([]MailServer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+mailServerColumns+`
		FROM mail_servers m JOIN servers sv ON sv.id = m.server_id ORDER BY sv.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MailServer, error) { return scanMailServer(r) })
}

// SetMailServer turns email on for a server, or changes its host name or TLS.
func (s *Store) SetMailServer(ctx context.Context, serverID, hostname, tls string) (MailServer, error) {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mail_servers (server_id, hostname, tls) VALUES ($1, $2, $3)
		ON CONFLICT (server_id) DO UPDATE SET hostname = $2, tls = $3, state = 'pending', message = NULL, updated_at = now()`,
		serverID, hostname, tls)
	if isUniqueViolation(err) {
		return MailServer{}, ErrMailHostTaken
	}
	if err != nil {
		return MailServer{}, err
	}
	// Every domain's MX record names the host name.
	if _, err := s.pool.Exec(ctx, `
		UPDATE mail_domains SET generation = generation + 1, state = CASE WHEN state = 'deleting' THEN state ELSE 'pending' END,
			updated_at = now()
		WHERE server_id = $1`, serverID); err != nil {
		return MailServer{}, err
	}
	return s.GetMailServer(ctx, serverID)
}

// DeleteMailServer turns email off. It fails with ErrInUse while the server has domains.
func (s *Store) DeleteMailServer(ctx context.Context, serverID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM mail_servers m WHERE server_id = $1
		AND NOT EXISTS (SELECT 1 FROM mail_domains d WHERE d.server_id = m.server_id)`, serverID)
	if err == nil && tag.RowsAffected() == 0 {
		if _, gerr := s.GetMailServer(ctx, serverID); gerr == nil {
			return ErrInUse
		}
		return ErrNotFound
	}
	return err
}

func (s *Store) MailServerResult(ctx context.Context, serverID string, ok bool, message string) error {
	state, msg := "active", (*string)(nil)
	if !ok {
		state, msg = "error", &message
	}
	_, err := s.pool.Exec(ctx, `UPDATE mail_servers SET state = $2, message = $3 WHERE server_id = $1`, serverID, state, msg)
	return err
}

// ---- Mail domains ----

type MailDomain struct {
	ID                string                     `json:"id"`
	ServerID          string                     `json:"server_id"`
	ServerName        string                     `json:"server_name"`
	Hostname          string                     `json:"hostname"` // the server's mail host name
	Domain            string                     `json:"domain"`
	CatchAll          *string                    `json:"catch_all"`
	State             string                     `json:"state"`
	Message           *string                    `json:"message"`
	DNSRecords        []agentproto.MailDNSRecord `json:"dns_records"`
	Generation        int64                      `json:"generation"`
	AppliedGeneration int64                      `json:"applied_generation"`
	OwnerID           *string                    `json:"owner_id"`
	OwnerName         *string                    `json:"owner_name"`
	Mailboxes         int                        `json:"mailboxes"`
	Forwarders        int                        `json:"forwarders"`
	CreatedAt         time.Time                  `json:"created_at"`
	UpdatedAt         time.Time                  `json:"updated_at"`
}

const mailDomainColumns = `d.id, d.server_id, sv.name, m.hostname, d.domain, d.catch_all, d.state, d.message, d.dns_records,
	d.generation, d.applied_generation, d.owner_id, ow.name,
	(SELECT count(*) FROM mailboxes b WHERE b.domain_id = d.id), (SELECT count(*) FROM mail_forwarders f WHERE f.domain_id = d.id),
	d.created_at, d.updated_at`

const mailDomainFrom = ` FROM mail_domains d JOIN mail_servers m ON m.server_id = d.server_id JOIN servers sv ON sv.id = d.server_id
	LEFT JOIN users ow ON ow.id = d.owner_id`

func scanMailDomain(row pgx.Row) (MailDomain, error) {
	var d MailDomain
	var records []byte
	err := row.Scan(&d.ID, &d.ServerID, &d.ServerName, &d.Hostname, &d.Domain, &d.CatchAll, &d.State, &d.Message, &records,
		&d.Generation, &d.AppliedGeneration, &d.OwnerID, &d.OwnerName, &d.Mailboxes, &d.Forwarders, &d.CreatedAt, &d.UpdatedAt)
	if err == nil {
		err = json.Unmarshal(records, &d.DNSRecords)
	}
	return d, notFound(err)
}

type NewMailDomain struct {
	ServerID  string
	Domain    string
	OwnerID   *string
	CreatedBy *string
}

func (s *Store) CreateMailDomain(ctx context.Context, n NewMailDomain) (MailDomain, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO mail_domains (server_id, domain, owner_id, created_by) VALUES ($1, $2, $3, $4) RETURNING id`,
		n.ServerID, n.Domain, n.OwnerID, n.CreatedBy).Scan(&id)
	if isUniqueViolation(err) {
		return MailDomain{}, ErrMailDomainTaken
	}
	if err != nil {
		return MailDomain{}, err
	}
	return s.GetMailDomain(ctx, id)
}

func (s *Store) GetMailDomain(ctx context.Context, id string) (MailDomain, error) {
	return scanMailDomain(s.pool.QueryRow(ctx, `SELECT `+mailDomainColumns+mailDomainFrom+` WHERE d.id = $1`, id))
}

// ListMailDomains returns the mail domains of one server (or all, serverID ""), of one
// owner only unless ownerID is "".
func (s *Store) ListMailDomains(ctx context.Context, serverID, ownerID string) ([]MailDomain, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+mailDomainColumns+mailDomainFrom+`
		WHERE ($1 = '' OR d.server_id::text = $1) AND ($2 = '' OR `+ownedBy("d.owner_id", "$2")+`) ORDER BY d.domain`, serverID, ownerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MailDomain, error) { return scanMailDomain(r) })
}

// bumpMailDomain marks a domain for re-applying after a change to it or its contents.
func bumpMailDomain(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, id string) error {
	tag, err := q.Exec(ctx, `
		UPDATE mail_domains SET generation = generation + 1, state = 'pending', message = NULL, updated_at = now()
		WHERE id = $1 AND state <> 'deleting'`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) SetMailCatchAll(ctx context.Context, id string, localPart *string) (MailDomain, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE mail_domains SET catch_all = $2 WHERE id = $1 AND state <> 'deleting'`, id, localPart); err != nil {
		return MailDomain{}, err
	}
	if err := bumpMailDomain(ctx, s.pool, id); err != nil {
		return MailDomain{}, err
	}
	return s.GetMailDomain(ctx, id)
}

func (s *Store) MarkMailDomainDeleting(ctx context.Context, id string) (MailDomain, error) {
	_, err := s.pool.Exec(ctx, `
		UPDATE mail_domains SET state = 'deleting', message = NULL, generation = generation + 1, updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return MailDomain{}, err
	}
	return s.GetMailDomain(ctx, id)
}

// ApplyMailDomainResult records what the agent reported for a domain at a generation. A
// deleted domain's row goes once the agent removed it.
func (s *Store) ApplyMailDomainResult(ctx context.Context, r agentproto.MailDomainResult) error {
	if r.OK {
		tag, err := s.pool.Exec(ctx, `DELETE FROM mail_domains WHERE id = $1 AND state = 'deleting' AND generation = $2`, r.ID, r.Generation)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		var records []byte
		if r.DNS != nil {
			records, _ = json.Marshal(r.DNS)
		}
		_, err = s.pool.Exec(ctx, `
			UPDATE mail_domains SET dns_records = COALESCE($3::jsonb, dns_records),
				state = CASE WHEN generation = $2 AND state <> 'deleting' THEN 'active' ELSE state END,
				message = CASE WHEN generation = $2 AND state <> 'deleting' THEN NULL ELSE message END,
				applied_generation = CASE WHEN generation = $2 THEN $2 ELSE applied_generation END
			WHERE id = $1`, r.ID, r.Generation, records)
		if err != nil {
			return err
		}
		for id, used := range r.Usage {
			if _, err := s.pool.Exec(ctx, `UPDATE mailboxes SET used_bytes = $3 WHERE id = $2 AND domain_id = $1`, r.ID, id, used); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE mail_domains SET message = $3, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE id = $1 AND generation = $2`, r.ID, r.Generation, r.Error)
	return err
}

// FailUnappliedMail mirrors FailUnappliedSites for a run that could not start.
func (s *Store) FailUnappliedMail(ctx context.Context, serverID, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE mail_domains SET message = $2, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE server_id = $1 AND generation > applied_generation`, serverID, message)
	return err
}

// ---- Mailboxes and forwarders ----

type Mailbox struct {
	ID           string                `json:"id"`
	DomainID     string                `json:"domain_id"`
	LocalPart    string                `json:"local_part"`
	QuotaMB      int                   `json:"quota_mb"`
	ForwardTo    []string              `json:"forward_to"`
	KeepCopy     bool                  `json:"keep_copy"`
	AutoReply    *agentproto.AutoReply `json:"auto_reply"`
	UsedBytes    *int64                `json:"used_bytes"`
	CreatedAt    time.Time             `json:"created_at"`
	PasswordHash string                `json:"-"`
}

const mailboxColumns = `id, domain_id, local_part, quota_mb, forward_to, keep_copy, auto_reply, used_bytes, created_at, password_hash`

func scanMailbox(row pgx.Row) (Mailbox, error) {
	var m Mailbox
	err := row.Scan(&m.ID, &m.DomainID, &m.LocalPart, &m.QuotaMB, &m.ForwardTo, &m.KeepCopy, &m.AutoReply, &m.UsedBytes, &m.CreatedAt, &m.PasswordHash)
	return m, notFound(err)
}

type MailForwarder struct {
	ID           string    `json:"id"`
	DomainID     string    `json:"domain_id"`
	LocalPart    string    `json:"local_part"`
	Destinations []string  `json:"destinations"`
	CreatedAt    time.Time `json:"created_at"`
}

const forwarderColumns = `id, domain_id, local_part, destinations, created_at`

func scanForwarder(row pgx.Row) (MailForwarder, error) {
	var f MailForwarder
	err := row.Scan(&f.ID, &f.DomainID, &f.LocalPart, &f.Destinations, &f.CreatedAt)
	return f, notFound(err)
}

func (s *Store) GetMailbox(ctx context.Context, id string) (Mailbox, error) {
	return scanMailbox(s.pool.QueryRow(ctx, `SELECT `+mailboxColumns+` FROM mailboxes WHERE id = $1`, id))
}

func (s *Store) GetMailForwarder(ctx context.Context, id string) (MailForwarder, error) {
	return scanForwarder(s.pool.QueryRow(ctx, `SELECT `+forwarderColumns+` FROM mail_forwarders WHERE id = $1`, id))
}

// MailboxesOn returns the mailboxes of one domain, or of every domain on a server (by
// domain ID) with serverID set instead.
func (s *Store) MailboxesOn(ctx context.Context, domainID, serverID string) ([]Mailbox, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+mailboxColumns+` FROM mailboxes
		WHERE ($1 = '' OR domain_id::text = $1)
		  AND ($2 = '' OR domain_id IN (SELECT id FROM mail_domains WHERE server_id::text = $2))
		ORDER BY local_part`, domainID, serverID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Mailbox, error) { return scanMailbox(r) })
}

func (s *Store) ForwardersOn(ctx context.Context, domainID, serverID string) ([]MailForwarder, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+forwarderColumns+` FROM mail_forwarders
		WHERE ($1 = '' OR domain_id::text = $1)
		  AND ($2 = '' OR domain_id IN (SELECT id FROM mail_domains WHERE server_id::text = $2))
		ORDER BY local_part`, domainID, serverID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MailForwarder, error) { return scanForwarder(r) })
}

// changeMailDomain runs fn in a transaction that holds the domain's row, so two changes to
// its addresses cannot race, and marks the domain for re-applying.
func (s *Store) changeMailDomain(ctx context.Context, domainID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM mail_domains WHERE id = $1 FOR UPDATE`, domainID).Scan(&state); err != nil {
		return notFound(err)
	}
	if state == "deleting" {
		return ErrNotFound
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := bumpMailDomain(ctx, tx, domainID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// addressFree fails with ErrMailAddressTaken if a mailbox or forwarder has the address.
func addressFree(ctx context.Context, tx pgx.Tx, domainID, localPart string) error {
	var used bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mailboxes WHERE domain_id = $1 AND local_part = $2)
		OR EXISTS (SELECT 1 FROM mail_forwarders WHERE domain_id = $1 AND local_part = $2)`, domainID, localPart).Scan(&used)
	if err == nil && used {
		return ErrMailAddressTaken
	}
	return err
}

func (s *Store) CreateMailbox(ctx context.Context, m Mailbox) (Mailbox, error) {
	var id string
	err := s.changeMailDomain(ctx, m.DomainID, func(tx pgx.Tx) error {
		if err := addressFree(ctx, tx, m.DomainID, m.LocalPart); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO mailboxes (domain_id, local_part, password_hash, quota_mb, forward_to, keep_copy, auto_reply)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			m.DomainID, m.LocalPart, m.PasswordHash, m.QuotaMB, nonNil(m.ForwardTo), m.KeepCopy, m.AutoReply).Scan(&id)
	})
	if err != nil {
		return Mailbox{}, err
	}
	return s.GetMailbox(ctx, id)
}

// SaveMailbox writes a mailbox's password hash, quota, forwarding and auto-reply.
func (s *Store) SaveMailbox(ctx context.Context, m Mailbox) (Mailbox, error) {
	err := s.changeMailDomain(ctx, m.DomainID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE mailboxes SET password_hash = $2, quota_mb = $3, forward_to = $4, keep_copy = $5, auto_reply = $6
			WHERE id = $1`, m.ID, m.PasswordHash, m.QuotaMB, nonNil(m.ForwardTo), m.KeepCopy, m.AutoReply)
		return err
	})
	if err != nil {
		return Mailbox{}, err
	}
	return s.GetMailbox(ctx, m.ID)
}

// DeleteMailbox removes a mailbox (its mail goes at the next apply), and the domain's
// catch-all if it pointed there.
func (s *Store) DeleteMailbox(ctx context.Context, m Mailbox) error {
	return s.changeMailDomain(ctx, m.DomainID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM mailboxes WHERE id = $1`, m.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE mail_domains SET catch_all = NULL WHERE id = $1 AND catch_all = $2`, m.DomainID, m.LocalPart)
		return err
	})
}

func (s *Store) CreateMailForwarder(ctx context.Context, f MailForwarder) (MailForwarder, error) {
	var id string
	err := s.changeMailDomain(ctx, f.DomainID, func(tx pgx.Tx) error {
		if err := addressFree(ctx, tx, f.DomainID, f.LocalPart); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO mail_forwarders (domain_id, local_part, destinations) VALUES ($1, $2, $3) RETURNING id`,
			f.DomainID, f.LocalPart, f.Destinations).Scan(&id)
	})
	if err != nil {
		return MailForwarder{}, err
	}
	return s.GetMailForwarder(ctx, id)
}

func (s *Store) SaveMailForwarder(ctx context.Context, f MailForwarder) (MailForwarder, error) {
	err := s.changeMailDomain(ctx, f.DomainID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE mail_forwarders SET destinations = $2 WHERE id = $1`, f.ID, f.Destinations)
		return err
	})
	if err != nil {
		return MailForwarder{}, err
	}
	return s.GetMailForwarder(ctx, f.ID)
}

func (s *Store) DeleteMailForwarder(ctx context.Context, f MailForwarder) error {
	return s.changeMailDomain(ctx, f.DomainID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM mail_forwarders WHERE id = $1`, f.ID)
		return err
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---- DNS ----

// MailTarget is a mail domain with the records it needs, for publishing to Cloudflare.
type MailTarget struct {
	Domain  string
	OwnerID *string
	Records []agentproto.MailDNSRecord
}

func (s *Store) MailTargets(ctx context.Context) ([]MailTarget, error) {
	rows, err := s.pool.Query(ctx, `SELECT domain, owner_id, dns_records FROM mail_domains WHERE state <> 'deleting' ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MailTarget, error) {
		var t MailTarget
		var records []byte
		if err := r.Scan(&t.Domain, &t.OwnerID, &records); err != nil {
			return t, err
		}
		return t, json.Unmarshal(records, &t.Records)
	})
}

// MailHostTargets are the mail host names, with the addresses of their servers.
func (s *Store) MailHostTargets(ctx context.Context) ([]HostTarget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.hostname, NULL::uuid, sv.name, host(sv.public_ipv4), host(sv.public_ipv6), sv.addresses
		FROM mail_servers m JOIN servers sv ON sv.id = m.server_id ORDER BY m.hostname`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[HostTarget])
}

// HostnameOwners returns the owner of each website or app that has the host name (nil
// for the administrators'), or nothing if none has it.
func (s *Store) HostnameOwners(ctx context.Context, hostnames []string) ([]*string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(st.owner_id, a.owner_id) FROM site_hostnames h
		LEFT JOIN sites st ON st.id = h.site_id LEFT JOIN apps a ON a.id = h.app_id
		WHERE h.hostname = ANY($1) AND COALESCE(st.state, a.state) <> 'deleting'`, hostnames)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[*string])
}

// MailDomainsTaken returns the names that are mail domains already.
func (s *Store) MailDomainsTaken(ctx context.Context, names []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT domain FROM mail_domains WHERE domain = ANY($1) ORDER BY domain`, names)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
