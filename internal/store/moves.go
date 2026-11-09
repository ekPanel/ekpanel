package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrMoveOpen: the site already has a move that is not finished.
	ErrMoveOpen = errors.New("this website is already being moved")
	// ErrMoveNameTaken: the target server already has a database with one of the names.
	ErrMoveNameTaken = errors.New("the target server already has a database with this name")
)

// MoveDatabase is a database as it was on the source of a move.
type MoveDatabase struct {
	ID       string `json:"id"`
	Engine   string `json:"engine"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

// Move is a website moving (or moved) to another server.
type Move struct {
	ID          string         `json:"id"`
	SiteID      *string        `json:"site_id"`
	AppID       *string        `json:"app_id"`
	FromServer  string         `json:"from_server"`
	ToServer    string         `json:"to_server"`
	FromName    string         `json:"from_server_name"`
	ToName      string         `json:"to_server_name"`
	DatabaseIDs []string       `json:"database_ids"`
	LinuxUser   string         `json:"-"`
	Databases   []MoveDatabase `json:"databases"`
	Phase       string         `json:"phase"`
	Error       *string        `json:"error"`
	Bytes       int64          `json:"bytes"`
	CheckStatus *int           `json:"check_status"`
	// TargetCleanup: the target still has a half-made copy to remove.
	TargetCleanup bool       `json:"target_cleanup"`
	CreatedBy     *string    `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	MovedAt       *time.Time `json:"moved_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

const moveColumns = `m.id, m.site_id, m.app_id, m.from_server, m.to_server, COALESCE(fs.name, ''), COALESCE(ts.name, ''), m.database_ids,
	m.linux_user, m.databases, m.phase, m.error, m.bytes, m.check_status, m.target_cleanup, m.created_by, m.created_at, m.moved_at, m.finished_at`

const moveFrom = ` FROM moves m LEFT JOIN servers fs ON fs.id = m.from_server LEFT JOIN servers ts ON ts.id = m.to_server`

func scanMove(row pgx.Row) (Move, error) {
	var m Move
	err := row.Scan(&m.ID, &m.SiteID, &m.AppID, &m.FromServer, &m.ToServer, &m.FromName, &m.ToName, &m.DatabaseIDs,
		&m.LinuxUser, &m.Databases, &m.Phase, &m.Error, &m.Bytes, &m.CheckStatus, &m.TargetCleanup, &m.CreatedBy, &m.CreatedAt, &m.MovedAt, &m.FinishedAt)
	if m.Databases == nil {
		m.Databases = []MoveDatabase{}
	}
	return m, notFound(err)
}

func (s *Store) collectMoves(rows pgx.Rows, err error) ([]Move, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Move{}
	for rows.Next() {
		m, err := scanMove(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateMove starts a move. ErrConflict: the site already has one that is not finished.
func (s *Store) CreateMove(ctx context.Context, m Move) (Move, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO moves (site_id, app_id, from_server, to_server, database_ids, linux_user, databases, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		m.SiteID, m.AppID, m.FromServer, m.ToServer, m.DatabaseIDs, m.LinuxUser, m.Databases, m.CreatedBy).Scan(&id)
	if isUniqueViolation(err) {
		return Move{}, ErrMoveOpen
	}
	if err != nil {
		return Move{}, err
	}
	return s.GetMove(ctx, id)
}

func (s *Store) GetMove(ctx context.Context, id string) (Move, error) {
	return scanMove(s.pool.QueryRow(ctx, `SELECT `+moveColumns+moveFrom+` WHERE m.id = $1`, id))
}

// AppMoves lists an app's moves, newest first.
func (s *Store) AppMoves(ctx context.Context, appID string) ([]Move, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+moveColumns+moveFrom+` WHERE m.app_id = $1 ORDER BY m.created_at DESC LIMIT 20`, appID)
	return s.collectMoves(rows, err)
}

// OpenAppMove is the app's move that is not finished, if any.
func (s *Store) OpenAppMove(ctx context.Context, appID string) (Move, error) {
	return scanMove(s.pool.QueryRow(ctx, `SELECT `+moveColumns+moveFrom+`
		WHERE m.app_id = $1 AND m.phase IN ('copying', 'switching', 'moved', 'cleaning')`, appID))
}

// SiteMoves lists a site's moves, newest first.
func (s *Store) SiteMoves(ctx context.Context, siteID string) ([]Move, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+moveColumns+moveFrom+` WHERE m.site_id = $1 ORDER BY m.created_at DESC LIMIT 20`, siteID)
	return s.collectMoves(rows, err)
}

// MovesOn are the moves that concern what is on serverID: the source of a move (while it
// copies, and its old copy until removed), or a half-made copy on the target of a failed
// switch.
func (s *Store) MovesOn(ctx context.Context, serverID string) ([]Move, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+moveColumns+moveFrom+`
		WHERE (m.from_server = $1 AND m.phase IN ('copying', 'switching', 'moved', 'cleaning'))
		   OR (m.to_server = $1 AND m.target_cleanup)`, serverID)
	return s.collectMoves(rows, err)
}

// SetTargetCleanup marks (or clears) the target's half-made copy for removal.
func (s *Store) SetTargetCleanup(ctx context.Context, id string, on bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE moves SET target_cleanup = $2 WHERE id = $1`, id, on)
	return err
}

// OpenMove is the site's move that is not finished, if any.
func (s *Store) OpenMove(ctx context.Context, siteID string) (Move, error) {
	return scanMove(s.pool.QueryRow(ctx, `SELECT `+moveColumns+moveFrom+`
		WHERE m.site_id = $1 AND m.phase IN ('copying', 'switching', 'moved', 'cleaning')`, siteID))
}

// SetMovePhase records a move's progress. An error message is kept until it is replaced.
func (s *Store) SetMovePhase(ctx context.Context, id, phase string, errMsg *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE moves SET phase = $2, error = $3,
			moved_at = CASE WHEN $2 = 'moved' THEN now() ELSE moved_at END,
			finished_at = CASE WHEN $2 IN ('done', 'failed') THEN now() ELSE finished_at END
		WHERE id = $1`, id, phase, errMsg)
	return err
}

func (s *Store) SetMoveCopied(ctx context.Context, id string, bytes int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE moves SET bytes = $2 WHERE id = $1`, id, bytes)
	return err
}

func (s *Store) SetMoveCheck(ctx context.Context, id string, status int) error {
	_, err := s.pool.Exec(ctx, `UPDATE moves SET check_status = $2 WHERE id = $1`, id, status)
	return err
}

// SwitchMove gives the site (or app) and its databases to the target server, in one
// transaction, and bumps their generations so the target applies them. An app gets a free
// port there. ErrMoveNameTaken: the target already has a database or app with the name.
func (s *Store) SwitchMove(ctx context.Context, m Move) error {
	return s.moveTo(ctx, m, m.ToServer, true)
}

// UnswitchMove gives the site (or app) and its databases back to the source.
func (s *Store) UnswitchMove(ctx context.Context, m Move) error {
	return s.moveTo(ctx, m, m.FromServer, false)
}

// freePortSQL is the lowest free app port on server $2 for app $1 (two in a row for Git apps).
const freePortSQL = `(SELECT p FROM generate_series(20000, 29998) p
	 WHERE p NOT IN (SELECT u FROM apps a, LATERAL (VALUES (a.host_port), (CASE WHEN a.kind = 'git' THEN a.host_port + 1 END)) v(u)
	                 WHERE a.server_id = $2 AND a.id <> $1 AND u IS NOT NULL)
	   AND (x.kind <> 'git' OR p + 1 NOT IN (SELECT u FROM apps a, LATERAL (VALUES (a.host_port), (CASE WHEN a.kind = 'git' THEN a.host_port + 1 END)) v(u)
	                 WHERE a.server_id = $2 AND a.id <> $1 AND u IS NOT NULL))
	 ORDER BY p LIMIT 1)`

func (s *Store) moveTo(ctx context.Context, m Move, server string, switching bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if m.SiteID != nil {
		if _, err := tx.Exec(ctx, `UPDATE sites SET server_id = $2, generation = generation + 1, state = 'pending', updated_at = now()
			WHERE id = $1`, m.SiteID, server); err != nil {
			return err
		}
	}
	if m.AppID != nil {
		_, err := tx.Exec(ctx, `UPDATE apps x SET server_id = $2, generation = generation + 1, state = 'pending', updated_at = now(),
			host_port = CASE WHEN x.host_port IS NULL THEN NULL ELSE `+freePortSQL+` END
			WHERE x.id = $1`, m.AppID, server)
		if isUniqueViolation(err) {
			return ErrMoveNameTaken
		}
		if err != nil {
			return err
		}
	}
	if len(m.DatabaseIDs) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE databases SET server_id = $2, generation = generation + 1, state = 'pending', updated_at = now()
			WHERE id = ANY($1)`, m.DatabaseIDs, server); err != nil {
			if isUniqueViolation(err) {
				return ErrMoveNameTaken
			}
			return err
		}
	}
	if switching {
		if _, err := tx.Exec(ctx, `UPDATE moves SET phase = 'switching' WHERE id = $1`, m.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// InterruptedMoves are moves that were copying or switching (only right after the panel
// starts, when no move task can be running).
func (s *Store) InterruptedMoves(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM moves WHERE phase IN ('copying', 'switching')`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
