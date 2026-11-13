package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// SuspendUser suspends a customer or a reseller (role) and ends their own sessions (an
// administrator signed in as them keeps theirs; a reseller's sessions as one of their
// customers end too). It reports ErrNotFound for anyone else. by is who suspends.
func (s *Store) SuspendUser(ctx context.Context, id, role, reason, by string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE users SET suspended_at = COALESCE(suspended_at, now()), suspend_reason = $3,
			suspended_by = $4, updated_at = now()
		WHERE id = $1 AND role = $2`, id, role, reason, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE (user_id = $1 AND impersonator_id IS NULL) OR impersonator_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) UnsuspendUser(ctx context.Context, id, role string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET suspended_at = NULL, suspend_reason = NULL, suspended_by = NULL, updated_at = now()
		WHERE id = $1 AND role = $2`, id, role)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// SuspendedByAdmin reports whether an administrator suspended the account itself.
func (s *Store) SuspendedByAdmin(ctx context.Context, id string) (bool, error) {
	var admin bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users u JOIN users b ON b.id = u.suspended_by
		WHERE u.id = $1 AND u.suspended_at IS NOT NULL AND b.role = 'admin')`, id).Scan(&admin)
	return admin, err
}

// SetBandwidthBlocked records whether a customer's websites are stopped for bandwidth and
// reports whether that changed.
func (s *Store) SetBandwidthBlocked(ctx context.Context, id string, blocked bool) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET bandwidth_blocked = $2 WHERE id = $1 AND bandwidth_blocked <> $2`, id, blocked)
	return tag.RowsAffected() > 0, err
}

// BandwidthCustomers lists the customers whose package has a bandwidth limit, and those
// still marked as blocked (their package may have lost its limit).
func (s *Store) BandwidthCustomers(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id FROM users u JOIN packages p ON p.id = u.package_id
		WHERE u.role = 'customer' AND (p.bandwidth_gb IS NOT NULL OR u.bandwidth_blocked)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// OwnerChange gives websites, databases, apps, email domains and hosted DNS zones to
// another owner: a customer, or the administrators (To nil).
type OwnerChange struct {
	To          *string
	Sites       []string
	Databases   []string
	Apps        []string
	MailDomains []string
	Zones       []string
}

// ChangeOwner applies an owner change in one transaction and returns the servers whose
// state changed. Things being deleted are left alone; if any listed item is missing (or
// being deleted), nothing changes and ErrNotFound is returned.
func (s *Store) ChangeOwner(ctx context.Context, c OwnerChange) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	servers := map[string]bool{}
	for _, t := range []struct {
		table string
		ids   []string
	}{{"sites", c.Sites}, {"databases", c.Databases}, {"apps", c.Apps}, {"mail_domains", c.MailDomains}} {
		if len(t.ids) == 0 {
			continue
		}
		rows, err := tx.Query(ctx, `UPDATE `+t.table+` SET owner_id = $2 WHERE id = ANY($1::uuid[]) AND state <> 'deleting'
			RETURNING server_id::text`, t.ids, c.To)
		if err != nil {
			return nil, err
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		if len(got) != len(t.ids) {
			return nil, ErrNotFound
		}
		for _, id := range got {
			servers[id] = true
		}
	}
	if len(c.Zones) > 0 {
		tag, err := tx.Exec(ctx, `UPDATE hosted_zones SET owner_id = $2 WHERE id = ANY($1::uuid[])`, c.Zones, c.To)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() != int64(len(c.Zones)) {
			return nil, ErrNotFound
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(servers))
	for id := range servers {
		out = append(out, id)
	}
	return out, nil
}

// SuspendedCustomers is the set of suspended customers, those of suspended resellers too.
func (s *Store) SuspendedCustomers(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id::text FROM users u LEFT JOIN users r ON r.id = u.reseller_id
		WHERE u.suspended_at IS NOT NULL OR r.suspended_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}
