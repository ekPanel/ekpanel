package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"ekpanel/internal/agentproto"
)

// ServerSecurity is a server's security settings and what its agent last reported.
type ServerSecurity struct {
	CrowdSec          bool                       `json:"crowdsec"`
	Allowlist         []string                   `json:"allowlist"`
	Countries         agentproto.CountryBlock    `json:"countries"`
	MalwareScan       string                     `json:"malware_scan"`
	MalwareQuarantine bool                       `json:"malware_quarantine"`
	MalwareScannedAt  *time.Time                 `json:"malware_scanned_at"`
	MalwareLast       *MalwareScanSummary        `json:"malware_last"`
	Status            *agentproto.SecurityResult `json:"status"`
	CheckedAt         *time.Time                 `json:"checked_at"`
}

func (s *Store) GetSecurity(ctx context.Context, serverID string) (ServerSecurity, error) {
	var sec ServerSecurity
	err := s.pool.QueryRow(ctx, `SELECT crowdsec, allowlist, country_mode, countries, malware_scan, malware_quarantine, malware_scanned_at,
		malware_last, security_status, security_checked_at FROM servers WHERE id = $1`, serverID).
		Scan(&sec.CrowdSec, &sec.Allowlist, &sec.Countries.Mode, &sec.Countries.Countries, &sec.MalwareScan, &sec.MalwareQuarantine,
			&sec.MalwareScannedAt, &sec.MalwareLast, &sec.Status, &sec.CheckedAt)
	return sec, notFound(err)
}

// Spec is what the agent is sent.
func (sec ServerSecurity) Spec() *agentproto.SecuritySpec {
	return &agentproto.SecuritySpec{CrowdSec: sec.CrowdSec, Allowlist: sec.Allowlist, Countries: sec.Countries,
		Malware: sec.MalwareScan != agentproto.MalwareOff}
}

func (s *Store) SetCountries(ctx context.Context, serverID string, c agentproto.CountryBlock) error {
	if c.Countries == nil {
		c.Countries = []string{}
	}
	tag, err := s.pool.Exec(ctx, `UPDATE servers SET country_mode = $2, countries = $3 WHERE id = $1`, serverID, c.Mode, c.Countries)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) SetMalwareScan(ctx context.Context, serverID, schedule string, quarantine bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE servers SET malware_scan = $2, malware_quarantine = $3 WHERE id = $1`, serverID, schedule, quarantine)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) SetSecurity(ctx context.Context, serverID string, crowdsec bool, allowlist []string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE servers SET crowdsec = $2, allowlist = $3 WHERE id = $1`, serverID, crowdsec, allowlist)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) RecordSecurityStatus(ctx context.Context, serverID string, res agentproto.SecurityResult) error {
	_, err := s.pool.Exec(ctx, `UPDATE servers SET security_status = $2, security_checked_at = now() WHERE id = $1`, serverID, res)
	return err
}

// AdminsWithout2FA lists the administrators who sign in with a password only.
func (s *Store) AdminsWithout2FA(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT email FROM users WHERE role = 'admin' AND NOT totp_enabled ORDER BY email`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
