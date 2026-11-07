package store

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/jackc/pgx/v5"
)

type Server struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	EnrolledAt      *time.Time `json:"enrolled_at"`
	EnrollExpiresAt *time.Time `json:"enroll_expires_at"`
	Hostname        string     `json:"hostname"`
	OS              string     `json:"os"`
	Kernel          string     `json:"kernel"`
	Arch            string     `json:"arch"`
	CPUCount        int        `json:"cpu_count"`
	AgentVersion    string     `json:"agent_version"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

const serverColumns = `id, name, enrolled_at, enroll_expires_at, hostname, os, kernel, arch, cpu_count, agent_version, last_seen_at, created_at`

func scanServer(row pgx.Row) (Server, error) {
	var s Server
	err := row.Scan(&s.ID, &s.Name, &s.EnrolledAt, &s.EnrollExpiresAt, &s.Hostname, &s.OS, &s.Kernel, &s.Arch,
		&s.CPUCount, &s.AgentVersion, &s.LastSeenAt, &s.CreatedAt)
	return s, notFound(err)
}

const EnrollTokenTTL = time.Hour

// CreateServer registers a server that is waiting for its agent to enrol with the token
// whose hash is enrollHash.
func (s *Store) CreateServer(ctx context.Context, name string, enrollHash []byte) (Server, error) {
	return scanServer(s.pool.QueryRow(ctx, `
		INSERT INTO servers (name, enroll_token_hash, enroll_expires_at)
		VALUES ($1, $2, now() + $3 * interval '1 second')
		RETURNING `+serverColumns, name, enrollHash, int64(EnrollTokenTTL.Seconds())))
}

// RenewEnrollToken replaces the enrolment token of a server, for example after it expired.
// It also revokes the current agent certificate, so a re-enrolled agent replaces the old one.
func (s *Store) RenewEnrollToken(ctx context.Context, id string, enrollHash []byte) (Server, error) {
	return scanServer(s.pool.QueryRow(ctx, `
		UPDATE servers SET enroll_token_hash = $2, enroll_expires_at = now() + $3 * interval '1 second',
			agent_cert_sha256 = NULL, agent_cert_expires_at = NULL
		WHERE id = $1
		RETURNING `+serverColumns, id, enrollHash, int64(EnrollTokenTTL.Seconds())))
}

func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+serverColumns+` FROM servers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Server, error) { return scanServer(r) })
}

func (s *Store) GetServer(ctx context.Context, id string) (Server, error) {
	return scanServer(s.pool.QueryRow(ctx, `SELECT `+serverColumns+` FROM servers WHERE id = $1`, id))
}

func (s *Store) DeleteServer(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM servers WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// IssueFunc signs an agent certificate for a server ID and returns its fingerprint and expiry.
type IssueFunc func(serverID string) (fingerprint string, expires time.Time, err error)

// EnrollServer consumes a valid enrolment token and records the certificate that issue
// creates for the server. The token works once: the row stays locked until it is cleared.
func (s *Store) EnrollServer(ctx context.Context, tokenHash []byte, issue IssueFunc) (Server, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Server{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `
		SELECT id FROM servers WHERE enroll_token_hash = $1 AND enroll_expires_at > now() FOR UPDATE`, tokenHash).Scan(&id)
	if err != nil {
		return Server{}, notFound(err)
	}
	fp, expires, err := issue(id)
	if err != nil {
		return Server{}, err
	}
	srv, err := scanServer(tx.QueryRow(ctx, `
		UPDATE servers
		SET enroll_token_hash = NULL, enroll_expires_at = NULL, enrolled_at = now(),
		    agent_cert_sha256 = $2, agent_cert_expires_at = $3
		WHERE id = $1
		RETURNING `+serverColumns, id, fp, expires))
	if err != nil {
		return Server{}, err
	}
	return srv, tx.Commit(ctx)
}

// AuthenticateAgent accepts a verified client certificate only if it is the one the panel
// last issued to that server.
func (s *Store) AuthenticateAgent(ctx context.Context, id, fingerprint string) (Server, error) {
	var stored *string
	err := s.pool.QueryRow(ctx, `SELECT agent_cert_sha256 FROM servers WHERE id = $1`, id).Scan(&stored)
	if err != nil {
		return Server{}, notFound(err)
	}
	if stored == nil || subtle.ConstantTimeCompare([]byte(*stored), []byte(fingerprint)) != 1 {
		return Server{}, ErrNotFound
	}
	return s.GetServer(ctx, id)
}

// ReplaceAgentCert swaps an agent's certificate during renewal. It only succeeds while
// oldFingerprint is still current, so a revoked agent cannot renew itself back in.
func (s *Store) ReplaceAgentCert(ctx context.Context, id, oldFingerprint, newFingerprint string, expires time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE servers SET agent_cert_sha256 = $3, agent_cert_expires_at = $4
		WHERE id = $1 AND agent_cert_sha256 = $2`, id, oldFingerprint, newFingerprint, expires)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

type ServerFacts struct {
	Hostname     string
	OS           string
	Kernel       string
	Arch         string
	CPUCount     int
	AgentVersion string
}

func (s *Store) UpdateServerFacts(ctx context.Context, id string, f ServerFacts) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE servers SET hostname = $2, os = $3, kernel = $4, arch = $5, cpu_count = $6, agent_version = $7, last_seen_at = now()
		WHERE id = $1`, id, f.Hostname, f.OS, f.Kernel, f.Arch, f.CPUCount, f.AgentVersion)
	return err
}

func (s *Store) TouchServer(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE servers SET last_seen_at = now() WHERE id = $1`, id)
	return err
}
