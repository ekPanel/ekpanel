package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// APIToken is a user's key for the API. The token itself is only shown when it is made.
type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"-"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	LastUsedIP string     `json:"last_used_ip"`
	CreatedAt  time.Time  `json:"created_at"`
}

const tokenColumns = `id, user_id, name, prefix, scopes, expires_at, last_used_at, last_used_ip, created_at`

func scanToken(row pgx.Row) (APIToken, error) {
	var t APIToken
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Scopes, &t.ExpiresAt, &t.LastUsedAt, &t.LastUsedIP, &t.CreatedAt)
	return t, notFound(err)
}

// MaxTokensPerUser bounds how many tokens one user can have.
const MaxTokensPerUser = 50

func (s *Store) CreateAPIToken(ctx context.Context, userID, name, prefix string, hash []byte, scopes []string, expiresAt *time.Time) (APIToken, error) {
	return scanToken(s.pool.QueryRow(ctx, `
		INSERT INTO api_tokens (user_id, name, token_hash, prefix, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+tokenColumns, userID, name, hash, prefix, scopes, expiresAt))
}

func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tokenColumns+` FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) CountAPITokens(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}

// DeleteAPIToken revokes one of a user's tokens.
func (s *Store) DeleteAPIToken(ctx context.Context, userID, id string) (APIToken, error) {
	return scanToken(s.pool.QueryRow(ctx, `DELETE FROM api_tokens WHERE id = $1 AND user_id = $2 RETURNING `+tokenColumns, id, userID))
}

// TokenUser finds the user a valid (unexpired) token belongs to, and notes that it was
// used (at most once a minute, so busy scripts do not write on every request).
func (s *Store) TokenUser(ctx context.Context, hash []byte, ip string) (User, APIToken, error) {
	t, err := scanToken(s.pool.QueryRow(ctx, `SELECT `+tokenColumns+` FROM api_tokens
		WHERE token_hash = $1 AND (expires_at IS NULL OR expires_at > now())`, hash))
	if err != nil {
		return User{}, APIToken{}, err
	}
	u, err := s.UserByID(ctx, t.UserID)
	if err != nil {
		return User{}, APIToken{}, err
	}
	_, err = s.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = now(), last_used_ip = $2
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute' OR last_used_ip <> $2)`, t.ID, ip)
	return u, t, err
}
