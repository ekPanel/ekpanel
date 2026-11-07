package store

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Deployment is one build and release of a Git app, or a rollback to an earlier build.
type Deployment struct {
	ID            string     `json:"id"`
	AppID         string     `json:"app_id"`
	Number        int        `json:"number"`
	Status        string     `json:"status"`
	Trigger       string     `json:"trigger"`
	Branch        string     `json:"branch"`
	CommitSHA     *string    `json:"commit_sha"`
	CommitMessage *string    `json:"commit_message"`
	Image         *string    `json:"image"`
	RollbackOf    *string    `json:"rollback_of"`
	Error         *string    `json:"error"`
	CreatedBy     *string    `json:"created_by"`
	CreatedByName *string    `json:"created_by_name"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	// RollbackPossible is true when the build is still kept on the server.
	RollbackPossible bool `json:"rollback_possible"`
}

// KeptBuilds is how many live builds of an app stay on its server for rollbacks.
const KeptBuilds = 5

// MaxDeploymentLog bounds a deployment's stored log; output after it is dropped.
const MaxDeploymentLog = 2 << 20

const deploymentColumns = `d.id, d.app_id, d.number, d.status, d.trigger, d.branch, d.commit_sha, d.commit_message, d.image,
	d.rollback_of, d.error, d.created_by, u.name, d.created_at, d.started_at, d.finished_at`

const deploymentFrom = ` FROM deployments d LEFT JOIN users u ON u.id = d.created_by`

func scanDeployment(row pgx.Row) (Deployment, error) {
	var d Deployment
	err := row.Scan(&d.ID, &d.AppID, &d.Number, &d.Status, &d.Trigger, &d.Branch, &d.CommitSHA, &d.CommitMessage, &d.Image,
		&d.RollbackOf, &d.Error, &d.CreatedBy, &d.CreatedByName, &d.CreatedAt, &d.StartedAt, &d.FinishedAt)
	return d, notFound(err)
}

type NewDeployment struct {
	AppID         string
	Trigger       string
	Branch        string
	CommitSHA     *string
	CommitMessage *string
	Image         *string // rollbacks: the build to return to
	RollbackOf    *string
	CreatedBy     *string
}

// CreateDeployment adds a queued deployment with the app's next number.
func (s *Store) CreateDeployment(ctx context.Context, n NewDeployment) (Deployment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deployment{}, err
	}
	defer tx.Rollback(ctx)
	// Lock the app so two deployments never take the same number.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM apps WHERE id = $1 FOR UPDATE`, n.AppID); err != nil {
		return Deployment{}, err
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO deployments (app_id, number, trigger, branch, commit_sha, commit_message, image, rollback_of, created_by)
		VALUES ($1, COALESCE((SELECT max(number) FROM deployments WHERE app_id = $1), 0) + 1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`, n.AppID, n.Trigger, n.Branch, n.CommitSHA, n.CommitMessage, n.Image, n.RollbackOf, n.CreatedBy).Scan(&id)
	if err != nil {
		return Deployment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, err
	}
	return s.GetDeployment(ctx, id)
}

func (s *Store) GetDeployment(ctx context.Context, id string) (Deployment, error) {
	d, err := scanDeployment(s.pool.QueryRow(ctx, `SELECT `+deploymentColumns+deploymentFrom+` WHERE d.id = $1`, id))
	if err != nil {
		return d, err
	}
	kept, err := s.KeptImages(ctx, d.AppID)
	d.RollbackPossible = d.Image != nil && d.Status == "superseded" && slices.Contains(kept, *d.Image)
	return d, err
}

// ListDeployments returns an app's latest deployments, newest first.
func (s *Store) ListDeployments(ctx context.Context, appID string, limit int) ([]Deployment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+deploymentColumns+deploymentFrom+` WHERE d.app_id = $1 ORDER BY d.number DESC LIMIT $2`, appID, limit)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Deployment, error) { return scanDeployment(r) })
	if err != nil {
		return nil, err
	}
	kept, err := s.KeptImages(ctx, appID)
	for i, d := range list {
		list[i].RollbackPossible = d.Image != nil && d.Status == "superseded" && slices.Contains(kept, *d.Image)
	}
	return list, err
}

// KeptImages are the builds of an app's last live deployments, which its server keeps.
func (s *Store) KeptImages(ctx context.Context, appID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT image FROM (
			SELECT image, max(number) AS n FROM deployments
			WHERE app_id = $1 AND image IS NOT NULL AND status IN ('live', 'superseded') GROUP BY image
		) k ORDER BY n DESC LIMIT $2`, appID, KeptBuilds)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeploymentLog returns the log from a character offset on, and the log's full length.
func (s *Store) DeploymentLog(ctx context.Context, id string, offset int) (string, int, error) {
	var text string
	var length int
	err := s.pool.QueryRow(ctx, `SELECT substr(log, $2 + 1), length(log) FROM deployments WHERE id = $1`, id, max(offset, 0)).Scan(&text, &length)
	return text, length, notFound(err)
}

// AppendDeploymentLog adds output, until the log reaches MaxDeploymentLog.
func (s *Store) AppendDeploymentLog(ctx context.Context, id, text string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments SET log = CASE
			WHEN length(log) + length($2) <= $3 THEN log || $2
			WHEN length(log) < $3 THEN log || E'\n[the log is too long; the rest is not kept]\n'
			ELSE log END
		WHERE id = $1`, id, text, MaxDeploymentLog)
	return err
}

// StartDeployment marks a deployment as building (or releasing, for a rollback).
func (s *Store) StartDeployment(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE deployments SET status = $2, started_at = now(), error = NULL WHERE id = $1`, id, status)
	return err
}

// DeploymentBuilt records what was built; the deployment is now being released.
func (s *Store) DeploymentBuilt(ctx context.Context, id, commit, message, image string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments SET status = 'releasing', commit_sha = $2, commit_message = $3, image = $4 WHERE id = $1`,
		id, commit, message, image)
	return err
}

// FinishDeployment ends a deployment. A live one supersedes the app's previous live one.
func (s *Store) FinishDeployment(ctx context.Context, id, status, errText string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var appID string
	var e *string
	if errText != "" {
		e = &errText
	}
	err = tx.QueryRow(ctx, `UPDATE deployments SET status = $2, error = $3, finished_at = now() WHERE id = $1 RETURNING app_id`,
		id, status, e).Scan(&appID)
	if err != nil {
		return notFound(err)
	}
	if status == "live" {
		if _, err := tx.Exec(ctx, `UPDATE deployments SET status = 'superseded' WHERE app_id = $1 AND status = 'live' AND id <> $2`, appID, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// LiveDeployment returns the app's live deployment, if any.
func (s *Store) LiveDeployment(ctx context.Context, appID string) (Deployment, error) {
	return scanDeployment(s.pool.QueryRow(ctx, `SELECT `+deploymentColumns+deploymentFrom+`
		WHERE d.app_id = $1 AND d.status = 'live' ORDER BY d.number DESC LIMIT 1`, appID))
}

// FailInterruptedDeployments ends deployments left building or releasing by a panel that
// stopped. It runs at start-up, when no deployment can be running.
func (s *Store) FailInterruptedDeployments(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments SET status = 'failed', error = 'the panel restarted during the deploy', finished_at = now()
		WHERE status IN ('building', 'releasing')`)
	return err
}
