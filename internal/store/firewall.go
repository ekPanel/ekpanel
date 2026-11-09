package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type FirewallRule struct {
	ID        string    `json:"id"`
	ServerID  string    `json:"server_id"`
	Action    string    `json:"action"`   // "allow" or "deny"
	Protocol  string    `json:"protocol"` // "tcp", "udp" or "both"
	PortFrom  *int      `json:"port_from"`
	PortTo    *int      `json:"port_to"`
	Source    string    `json:"source"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

// FirewallStatus is what the agent last reported about the server's firewall.
type FirewallStatus struct {
	Active   bool      `json:"active"`
	SSHPorts []int     `json:"ssh_ports"`
	Error    string    `json:"error,omitempty"`
	At       time.Time `json:"at"`
}

type Firewall struct {
	Mode   string          `json:"mode"` // "unmanaged", "on" or "off"
	Status *FirewallStatus `json:"status"`
	Rules  []FirewallRule  `json:"rules"`
}

const MaxFirewallRules = 200

func (s *Store) GetFirewall(ctx context.Context, serverID string) (Firewall, error) {
	var fw Firewall
	var status []byte
	err := s.pool.QueryRow(ctx, `SELECT firewall, firewall_status FROM servers WHERE id = $1`, serverID).Scan(&fw.Mode, &status)
	if err != nil {
		return fw, notFound(err)
	}
	if status != nil {
		fw.Status = &FirewallStatus{}
		if err := json.Unmarshal(status, fw.Status); err != nil {
			return fw, err
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT id, server_id, action, protocol, port_from, port_to, source, note, created_at
		FROM firewall_rules WHERE server_id = $1 ORDER BY created_at, id`, serverID)
	if err != nil {
		return fw, err
	}
	fw.Rules, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (FirewallRule, error) {
		var r FirewallRule
		err := row.Scan(&r.ID, &r.ServerID, &r.Action, &r.Protocol, &r.PortFrom, &r.PortTo, &r.Source, &r.Note, &r.CreatedAt)
		return r, err
	})
	return fw, err
}

func (s *Store) SetFirewallMode(ctx context.Context, serverID, mode string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE servers SET firewall = $2 WHERE id = $1`, serverID, mode)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) RecordFirewallStatus(ctx context.Context, serverID string, st FirewallStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE servers SET firewall_status = $2 WHERE id = $1`, serverID, b)
	return err
}

type NewFirewallRule struct {
	ServerID  string
	Action    string
	Protocol  string
	PortFrom  *int
	PortTo    *int
	Source    string
	Note      string
	CreatedBy *string
}

// AddFirewallRule refuses to go past MaxFirewallRules for one server.
func (s *Store) AddFirewallRule(ctx context.Context, n NewFirewallRule) (FirewallRule, error) {
	r := FirewallRule{ServerID: n.ServerID, Action: n.Action, Protocol: n.Protocol, PortFrom: n.PortFrom, PortTo: n.PortTo, Source: n.Source, Note: n.Note}
	err := s.pool.QueryRow(ctx, `INSERT INTO firewall_rules (server_id, action, protocol, port_from, port_to, source, note, created_by)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8
		WHERE (SELECT count(*) FROM firewall_rules WHERE server_id = $1) < $9
		RETURNING id, created_at`,
		n.ServerID, n.Action, n.Protocol, n.PortFrom, n.PortTo, n.Source, n.Note, n.CreatedBy, MaxFirewallRules).Scan(&r.ID, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrLimit
	}
	return r, err
}

func (s *Store) DeleteFirewallRule(ctx context.Context, serverID, id string) (FirewallRule, error) {
	var r FirewallRule
	err := s.pool.QueryRow(ctx, `DELETE FROM firewall_rules WHERE id = $1 AND server_id = $2
		RETURNING id, server_id, action, protocol, port_from, port_to, source, note, created_at`, id, serverID).
		Scan(&r.ID, &r.ServerID, &r.Action, &r.Protocol, &r.PortFrom, &r.PortTo, &r.Source, &r.Note, &r.CreatedAt)
	return r, notFound(err)
}
