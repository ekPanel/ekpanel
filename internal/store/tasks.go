package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

type Task struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	ServerID    *string         `json:"server_id"`
	ServerName  *string         `json:"server_name"`
	Params      json.RawMessage `json:"params"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	Result      json.RawMessage `json:"result"`
	Error       *string         `json:"error"`
	CreatedBy   *string         `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   *time.Time      `json:"started_at"`
	FinishedAt  *time.Time      `json:"finished_at"`
}

const taskColumns = `t.id, t.kind, t.server_id, s.name, t.params, t.status, t.attempts, t.max_attempts,
	t.result, t.error, t.created_by, t.created_at, t.started_at, t.finished_at`

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Kind, &t.ServerID, &t.ServerName, &t.Params, &t.Status, &t.Attempts, &t.MaxAttempts,
		&t.Result, &t.Error, &t.CreatedBy, &t.CreatedAt, &t.StartedAt, &t.FinishedAt)
	return t, notFound(err)
}

type NewTask struct {
	Kind        string
	ServerID    *string
	Params      any
	MaxAttempts int
	CreatedBy   *string
	// DedupeKey, when set, makes CreateTask return the already-queued task with the same
	// key instead of adding another.
	DedupeKey string
}

func (s *Store) CreateTask(ctx context.Context, n NewTask) (Task, error) {
	if n.MaxAttempts < 1 {
		n.MaxAttempts = 1
	}
	params, err := json.Marshal(n.Params)
	if err != nil {
		return Task{}, err
	}
	if n.Params == nil {
		params = []byte("{}")
	}
	var dedupe *string
	if n.DedupeKey != "" {
		dedupe = &n.DedupeKey
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO tasks (kind, server_id, params, max_attempts, created_by, dedupe_key) VALUES ($1, $2, $3, $4, $5, $6)
		-- A new request joins the queued task, and also brings forward one that was waiting
		-- to retry, with a fresh set of attempts.
		ON CONFLICT (dedupe_key) WHERE status = 'queued' AND dedupe_key IS NOT NULL
		DO UPDATE SET run_after = LEAST(tasks.run_after, now()), attempts = 0, error = NULL
		RETURNING id`, n.Kind, n.ServerID, params, n.MaxAttempts, n.CreatedBy, dedupe).Scan(&id)
	if err != nil {
		return Task{}, err
	}
	return s.GetTask(ctx, id)
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	return scanTask(s.pool.QueryRow(ctx, `
		SELECT `+taskColumns+` FROM tasks t LEFT JOIN servers s ON s.id = t.server_id WHERE t.id = $1`, id))
}

type TaskFilter struct {
	ServerID   string // empty: all servers
	ActiveOnly bool   // queued or running
	Limit      int
}

func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+taskColumns+` FROM tasks t LEFT JOIN servers s ON s.id = t.server_id
		WHERE ($1 = '' OR t.server_id::text = $1)
		  AND (NOT $2 OR t.status IN ('queued', 'running'))
		ORDER BY t.created_at DESC LIMIT $3`, f.ServerID, f.ActiveOnly, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Task, error) { return scanTask(r) })
}

// ClaimTask takes the oldest runnable task and leases it to the caller for `lease`.
// SKIP LOCKED lets many workers (and later many panel processes) claim without blocking.
func (s *Store) ClaimTask(ctx context.Context, lease time.Duration) (Task, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		UPDATE tasks SET status = 'running', attempts = attempts + 1, started_at = now(),
			locked_until = now() + $1 * interval '1 second'
		WHERE id = (
			SELECT id FROM tasks WHERE status = 'queued' AND run_after <= now()
			ORDER BY run_after, created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id`, int64(lease.Seconds())).Scan(&id)
	if err != nil {
		return Task{}, notFound(err)
	}
	return s.GetTask(ctx, id)
}

// ExtendLease keeps a long-running task from being taken for a crashed one.
func (s *Store) ExtendLease(ctx context.Context, id string, lease time.Duration) error {
	_, err := s.pool.Exec(ctx, `UPDATE tasks SET locked_until = now() + $2 * interval '1 second' WHERE id = $1 AND status = 'running'`,
		id, int64(lease.Seconds()))
	return err
}

func (s *Store) FinishTask(ctx context.Context, id string, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE tasks SET status = 'succeeded', result = $2, error = NULL, finished_at = now(), locked_until = NULL
		WHERE id = $1`, id, b)
	return err
}

// FailTask records an error. While attempts remain the task is queued again after
// retryAfter; otherwise it is failed for good.
func (s *Store) FailTask(ctx context.Context, id, message string, retryAfter time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET error = $2, locked_until = NULL,
			status = CASE WHEN attempts < max_attempts THEN 'queued' ELSE 'failed' END,
			run_after = CASE WHEN attempts < max_attempts THEN now() + $3 * interval '1 second' ELSE run_after END,
			finished_at = CASE WHEN attempts < max_attempts THEN NULL ELSE now() END
		WHERE id = $1`, id, message, int64(retryAfter.Seconds()))
	return err
}

// RecoverTasks requeues (or fails) tasks whose worker stopped without finishing them,
// for example because the panel restarted mid-task.
func (s *Store) RecoverTasks(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks SET locked_until = NULL,
			error = 'interrupted: the panel stopped while this task was running',
			status = CASE WHEN attempts < max_attempts THEN 'queued' ELSE 'failed' END,
			finished_at = CASE WHEN attempts < max_attempts THEN NULL ELSE now() END
		WHERE status = 'running' AND locked_until < now()`)
	return tag.RowsAffected(), err
}

// DeleteOldTasks removes finished tasks after the retention period.
func (s *Store) DeleteOldTasks(ctx context.Context, olderThan time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM tasks WHERE status IN ('succeeded', 'failed') AND finished_at < now() - $1 * interval '1 second'`,
		int64(olderThan.Seconds()))
	return err
}
