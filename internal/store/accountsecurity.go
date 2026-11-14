package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- Two-factor recovery codes ----

// SetRecoveryCodes replaces an account's recovery codes (nil removes them).
func (s *Store) SetRecoveryCodes(ctx context.Context, userID string, hashes [][]byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM recovery_codes WHERE user_id = $1`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, h); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// UseRecoveryCode uses up an unused code; false when there is none such.
func (s *Store) UseRecoveryCode(ctx context.Context, userID string, hash []byte) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE recovery_codes SET used_at = now() WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, userID, hash)
	return tag.RowsAffected() == 1, err
}

// RecoveryCodesLeft counts an account's unused recovery codes.
func (s *Store) RecoveryCodesLeft(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM recovery_codes WHERE user_id = $1 AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

// ---- Sessions ----

// SessionInfo is a signed-in browser as the account page shows it.
type SessionInfo struct {
	ID           string    `json:"id"`
	IP           string    `json:"ip"`
	UserAgent    string    `json:"user_agent"`
	Method       string    `json:"method"` // password, passkey
	CreatedAt    time.Time `json:"created_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	Impersonated bool      `json:"impersonated"` // an administrator signed in as this account
	Current      bool      `json:"current"`
	TokenHash    []byte    `json:"-"`
}

func (s *Store) ListSessions(ctx context.Context, userID string) ([]SessionInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, ip, user_agent, method, created_at, last_seen_at, impersonator_id IS NOT NULL, token_hash
		FROM sessions WHERE user_id = $1 AND expires_at > now() ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SessionInfo, error) {
		var x SessionInfo
		err := r.Scan(&x.ID, &x.IP, &x.UserAgent, &x.Method, &x.CreatedAt, &x.LastSeenAt, &x.Impersonated, &x.TokenHash)
		return x, err
	})
}

// DeleteUserSession ends one of an account's sessions.
func (s *Store) DeleteUserSession(ctx context.Context, userID, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND id::text = $2`, userID, id)
	return tag.RowsAffected() == 1, err
}

// ---- Passkeys ----

// Passkey is a WebAuthn credential of an account. Credential is go-webauthn's, as JSON.
type Passkey struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	CreatedAt    time.Time       `json:"created_at"`
	LastUsedAt   *time.Time      `json:"last_used_at"`
	CredentialID []byte          `json:"-"`
	Credential   json.RawMessage `json:"-"`
}

// WebAuthnHandle returns the account's WebAuthn user handle, making one the first time.
func (s *Store) WebAuthnHandle(ctx context.Context, userID string) ([]byte, error) {
	var h []byte
	if err := s.pool.QueryRow(ctx, `SELECT webauthn_handle FROM users WHERE id = $1`, userID).Scan(&h); err != nil {
		return nil, notFound(err)
	}
	if len(h) > 0 {
		return h, nil
	}
	h = make([]byte, 32)
	rand.Read(h)
	if _, err := s.pool.Exec(ctx, `UPDATE users SET webauthn_handle = $2 WHERE id = $1 AND webauthn_handle IS NULL`, userID, h); err != nil {
		return nil, err
	}
	err := s.pool.QueryRow(ctx, `SELECT webauthn_handle FROM users WHERE id = $1`, userID).Scan(&h)
	return h, err
}

// UserByWebAuthnHandle finds the account a passkey sign-in names.
func (s *Store) UserByWebAuthnHandle(ctx context.Context, handle []byte) (User, error) {
	if len(handle) == 0 {
		return User{}, ErrNotFound
	}
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE webauthn_handle = $1`, handle))
}

func (s *Store) ListPasskeys(ctx context.Context, userID string) ([]Passkey, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, created_at, last_used_at, credential_id, credential
		FROM passkeys WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Passkey, error) {
		var p Passkey
		err := r.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.LastUsedAt, &p.CredentialID, &p.Credential)
		return p, err
	})
}

// ErrPasskeyExists: the authenticator is registered already.
var ErrPasskeyExists = errors.New("this passkey is added already")

func (s *Store) AddPasskey(ctx context.Context, userID, name string, credentialID []byte, credential json.RawMessage) (Passkey, error) {
	var p Passkey
	err := s.pool.QueryRow(ctx, `INSERT INTO passkeys (user_id, name, credential_id, credential) VALUES ($1, $2, $3, $4)
		RETURNING id, name, created_at, last_used_at`, userID, name, credentialID, credential).Scan(&p.ID, &p.Name, &p.CreatedAt, &p.LastUsedAt)
	if isUniqueViolation(err) {
		return p, ErrPasskeyExists
	}
	return p, err
}

// UsePasskey stores a passkey's credential after a sign-in (its counter and flags change).
func (s *Store) UsePasskey(ctx context.Context, credentialID []byte, credential json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `UPDATE passkeys SET credential = $2, last_used_at = now() WHERE credential_id = $1`, credentialID, credential)
	return err
}

func (s *Store) DeletePasskey(ctx context.Context, userID, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM passkeys WHERE user_id = $1 AND id::text = $2`, userID, id)
	return tag.RowsAffected() == 1, err
}

// DeletePasskeys removes all of an account's passkeys.
func (s *Store) DeletePasskeys(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM passkeys WHERE user_id = $1`, userID)
	return err
}
