// Package store is the panel's data access layer over PostgreSQL.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrAlreadySetUp   = errors.New("panel already has an administrator")
	ErrDuplicateEmail = errors.New("email already in use")
	ErrLimit          = errors.New("limit reached")
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---- Users ----

type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	Role         string    `json:"role"`
	Locale       string    `json:"locale"`
	TOTPEnabled  bool      `json:"totp_enabled"`
	CreatedAt    time.Time `json:"created_at"`
	PackageID    *string   `json:"package_id"`
	PasswordHash string    `json:"-"`
	TOTPSecret   *string   `json:"-"`
	// Set when an administrator is signed in as this user; only SessionUser fills it.
	ImpersonatorID *string `json:"impersonator_id,omitempty"`
	// Until when this session may do things that need the password again; SessionUser fills it.
	ConfirmedUntil *time.Time `json:"confirmed_until,omitempty"`
}

const userColumns = `id, email, name, role, locale, totp_enabled, created_at, password_hash, totp_secret, package_id`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Locale, &u.TOTPEnabled, &u.CreatedAt, &u.PasswordHash, &u.TOTPSecret, &u.PackageID)
	return u, notFound(err)
}

func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&exists)
	return exists, err
}

// CreateFirstAdmin creates the initial administrator, failing with ErrAlreadySetUp if any
// user exists. The table lock makes two simultaneous setup requests safe.
func (s *Store) CreateFirstAdmin(ctx context.Context, email, name, passwordHash, locale string) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return User{}, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&exists); err != nil {
		return User{}, err
	}
	if exists {
		return User{}, ErrAlreadySetUp
	}
	u, err := scanUser(tx.QueryRow(ctx, `
		INSERT INTO users (email, name, password_hash, role, locale)
		VALUES ($1, $2, $3, 'admin', $4)
		RETURNING `+userColumns, email, name, passwordHash, locale))
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit(ctx)
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE lower(email) = lower($1)`, email))
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

func (s *Store) SetUserLocale(ctx context.Context, id, locale string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET locale = $2, updated_at = now() WHERE id = $1`, id, locale)
	return err
}

// SetTOTP stores a pending or active TOTP secret. A nil secret turns 2FA off.
func (s *Store) SetTOTP(ctx context.Context, id string, secret *string, enabled bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET totp_secret = $2, totp_enabled = $3, updated_at = now() WHERE id = $1`, id, secret, enabled)
	return err
}

// ---- Sessions ----

const SessionTTL = 7 * 24 * time.Hour

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID, ip, userAgent string) error {
	return s.CreateSessionBy(ctx, tokenHash, userID, ip, userAgent, "password")
}

// CreateSessionBy starts a session, recording how the account signed in (password, passkey).
func (s *Store) CreateSessionBy(ctx context.Context, tokenHash []byte, userID, ip, userAgent, method string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, ip, user_agent, expires_at, method)
		VALUES ($1, $2, $3, $4, now() + $5 * interval '1 second', $6)`,
		tokenHash, userID, ip, userAgent, int64(SessionTTL.Seconds()), method)
	return err
}

// SessionUser returns the user for a live session and slides its expiry forward.
// The bookkeeping write happens at most every five minutes per session.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.role, u.locale, u.totp_enabled, u.created_at, u.password_hash, u.totp_secret,
			u.package_id, s.impersonator_id, CASE WHEN s.confirmed_until > now() THEN s.confirmed_until END
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`, tokenHash).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Locale,
		&u.TOTPEnabled, &u.CreatedAt, &u.PasswordHash, &u.TOTPSecret, &u.PackageID, &u.ImpersonatorID, &u.ConfirmedUntil)
	if err != nil {
		return User{}, notFound(err)
	}
	// Sessions of an administrator signed in as someone else never get longer.
	_, err = s.pool.Exec(ctx, `
		UPDATE sessions SET last_seen_at = now(), expires_at = now() + $2 * interval '1 second'
		WHERE token_hash = $1 AND last_seen_at < now() - interval '5 minutes' AND impersonator_id IS NULL`,
		tokenHash, int64(SessionTTL.Seconds()))
	return u, err
}

// ImpersonationTTL is how long an administrator can stay signed in as a customer.
const ImpersonationTTL = time.Hour

// CreateImpersonationSession signs an administrator in as another user for a short time.
func (s *Store) CreateImpersonationSession(ctx context.Context, tokenHash []byte, userID, impersonatorID, ip, userAgent string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, impersonator_id, ip, user_agent, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + $6 * interval '1 second')`,
		tokenHash, userID, impersonatorID, ip, userAgent, int64(ImpersonationTTL.Seconds()))
	return err
}

// ConfirmSession marks a session as having just given the password again.
func (s *Store) ConfirmSession(ctx context.Context, tokenHash []byte, until time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET confirmed_until = $2 WHERE token_hash = $1`, tokenHash, until)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

// DeleteOtherSessions signs the user out everywhere except the session keepHash.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID string, keepHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2`, userID, keepHash)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	return err
}

// ---- Audit log ----

type AuditEntry struct {
	ID        int64          `json:"id"`
	At        time.Time      `json:"at"`
	ActorID   *string        `json:"actor_id"`
	ActorName *string        `json:"actor_name"`
	ActorIP   string         `json:"actor_ip"`
	Action    string         `json:"action"`
	Target    string         `json:"target"`
	Details   map[string]any `json:"details"`
}

func (s *Store) AddAudit(ctx context.Context, actorID *string, actorIP, action, target string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log (actor_id, actor_ip, action, target, details) VALUES ($1, $2, $3, $4, $5)`,
		actorID, actorIP, action, target, details)
	return err
}

// ListAudit returns up to limit entries older than beforeID (0 = newest first page).
func (s *Store) ListAudit(ctx context.Context, beforeID int64, limit int) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.at, a.actor_id, u.name, a.actor_ip, a.action, a.target, a.details
		FROM audit_log a LEFT JOIN users u ON u.id = a.actor_id
		WHERE $1 = 0 OR a.id < $1
		ORDER BY a.id DESC LIMIT $2`, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		err := r.Scan(&e.ID, &e.At, &e.ActorID, &e.ActorName, &e.ActorIP, &e.Action, &e.Target, &e.Details)
		return e, err
	})
}
