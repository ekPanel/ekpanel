package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrDatabaseNameTaken means the server already has a database or account with that name.
type ErrDatabaseNameTaken struct{ Field string }

func (e ErrDatabaseNameTaken) Error() string {
	return "that " + e.Field + " is already used on this server"
}

type Database struct {
	ID                string    `json:"id"`
	ServerID          string    `json:"server_id"`
	ServerName        string    `json:"server_name"`
	Engine            string    `json:"engine"`
	Name              string    `json:"name"`
	Username          string    `json:"username"`
	State             string    `json:"state"`
	Message           *string   `json:"message"`
	SizeBytes         *int64    `json:"size_bytes"`
	Generation        int64     `json:"generation"`
	AppliedGeneration int64     `json:"applied_generation"`
	OwnerID           *string   `json:"owner_id"`
	OwnerName         *string   `json:"owner_name"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	PasswordHash      string    `json:"-"`
}

const databaseColumns = `d.id, d.server_id, sv.name, d.engine, d.name, d.username, d.state, d.message, d.size_bytes,
	d.generation, d.applied_generation, d.created_at, d.updated_at, d.password_hash, d.owner_id, ow.name`

const databaseFrom = ` FROM databases d JOIN servers sv ON sv.id = d.server_id LEFT JOIN users ow ON ow.id = d.owner_id`

func scanDatabase(row pgx.Row) (Database, error) {
	var d Database
	err := row.Scan(&d.ID, &d.ServerID, &d.ServerName, &d.Engine, &d.Name, &d.Username, &d.State, &d.Message, &d.SizeBytes,
		&d.Generation, &d.AppliedGeneration, &d.CreatedAt, &d.UpdatedAt, &d.PasswordHash, &d.OwnerID, &d.OwnerName)
	return d, notFound(err)
}

type NewDatabase struct {
	ServerID     string
	Engine       string
	Name         string
	Username     string
	PasswordHash string
	OwnerID      *string
	CreatedBy    *string
}

func (s *Store) CreateDatabase(ctx context.Context, n NewDatabase) (Database, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO databases (server_id, engine, name, username, password_hash, owner_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		n.ServerID, n.Engine, n.Name, n.Username, n.PasswordHash, n.OwnerID, n.CreatedBy).Scan(&id)
	if isUniqueViolation(err) {
		field := "database name"
		var nameTaken bool
		s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM databases WHERE server_id = $1 AND engine = $2 AND name = $3)`,
			n.ServerID, n.Engine, n.Name).Scan(&nameTaken)
		if !nameTaken {
			field = "user name"
		}
		return Database{}, ErrDatabaseNameTaken{Field: field}
	}
	if err != nil {
		return Database{}, err
	}
	return s.GetDatabase(ctx, id)
}

func (s *Store) GetDatabase(ctx context.Context, id string) (Database, error) {
	return scanDatabase(s.pool.QueryRow(ctx, `SELECT `+databaseColumns+databaseFrom+` WHERE d.id = $1`, id))
}

// ListDatabases returns databases on one server, or on all servers when serverID is empty.
func (s *Store) ListDatabases(ctx context.Context, serverID string) ([]Database, error) {
	return s.ListDatabasesOf(ctx, serverID, "")
}

// ListDatabasesOf is ListDatabases for one owner only, unless ownerID is empty.
func (s *Store) ListDatabasesOf(ctx context.Context, serverID, ownerID string) ([]Database, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+databaseColumns+databaseFrom+`
		WHERE ($1 = '' OR d.server_id::text = $1) AND ($2 = '' OR `+ownedBy("d.owner_id", "$2")+`) ORDER BY d.name`, serverID, ownerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Database, error) { return scanDatabase(r) })
}

// SetDatabasePassword replaces the stored password hash and marks the database for re-applying.
func (s *Store) SetDatabasePassword(ctx context.Context, id, hash string) (Database, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE databases SET password_hash = $2, generation = generation + 1, state = 'pending', message = NULL,
			updated_at = now()
		WHERE id = $1 AND state <> 'deleting'`, id, hash)
	if err != nil {
		return Database{}, err
	}
	if tag.RowsAffected() == 0 {
		return Database{}, ErrNotFound
	}
	return s.GetDatabase(ctx, id)
}

func (s *Store) MarkDatabaseDeleting(ctx context.Context, id string) (Database, error) {
	_, err := s.pool.Exec(ctx, `
		UPDATE databases SET state = 'deleting', message = NULL, generation = generation + 1, updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return Database{}, err
	}
	return s.GetDatabase(ctx, id)
}

// ApplyDatabaseResult records what the agent reported for one database at a generation.
func (s *Store) ApplyDatabaseResult(ctx context.Context, id string, generation int64, ok bool, message string, size *int64) error {
	if ok {
		tag, err := s.pool.Exec(ctx, `DELETE FROM databases WHERE id = $1 AND state = 'deleting' AND generation = $2`, id, generation)
		if err != nil || tag.RowsAffected() > 0 {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			UPDATE databases SET size_bytes = COALESCE($3, size_bytes),
				state = CASE WHEN generation = $2 AND state <> 'deleting' THEN 'active' ELSE state END,
				message = CASE WHEN generation = $2 AND state <> 'deleting' THEN NULL ELSE message END,
				applied_generation = CASE WHEN generation = $2 THEN $2 ELSE applied_generation END
			WHERE id = $1`, id, generation, size)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE databases SET message = $3, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE id = $1 AND generation = $2`, id, generation, message)
	return err
}

// FailUnappliedDatabases mirrors FailUnappliedSites for a run that could not start.
func (s *Store) FailUnappliedDatabases(ctx context.Context, serverID, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE databases SET message = $2, state = CASE WHEN state = 'deleting' THEN 'deleting' ELSE 'error' END
		WHERE server_id = $1 AND generation > applied_generation`, serverID, message)
	return err
}
