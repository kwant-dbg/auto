// Package queue is a Postgres-backed work queue (SCH-1, SCH-2): leased
// claims with SELECT … FOR UPDATE SKIP LOCKED, deduplicated enqueue, bounded
// retries with back-off, and a dead-letter state.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the subset of pgx used here, satisfied by *pgxpool.Pool and pgx.Tx,
// so tasks can be enqueued in the same transaction as domain writes.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// DefaultMaxAttempts allows two retries (docs/04 §5 rule 6).
const DefaultMaxAttempts = 3

// NewTask describes work to enqueue.
type NewTask struct {
	Type          string
	DedupeKey     string // empty disables deduplication
	Priority      int
	RunAfter      time.Time
	MaxAttempts   int
	Inputs        any
	OpportunityID *int64
	QuestionID    *int64
}

// Task is a claimed unit of work.
type Task struct {
	ID            int64
	Type          string
	DedupeKey     *string
	Priority      int
	Attempts      int
	MaxAttempts   int
	Inputs        json.RawMessage
	OpportunityID *int64
	QuestionID    *int64
}

// Enqueue inserts a task. If a task with the same dedupe key exists in any
// state, nothing is inserted and created is false.
func Enqueue(ctx context.Context, db DB, t NewTask) (id int64, created bool, err error) {
	if t.Type == "" {
		return 0, false, errors.New("queue: task type is required")
	}
	inputs, err := json.Marshal(t.Inputs)
	if err != nil {
		return 0, false, err
	}
	if t.Inputs == nil {
		inputs = []byte("{}")
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = DefaultMaxAttempts
	}
	runAfter := t.RunAfter
	if runAfter.IsZero() {
		runAfter = time.Now()
	}
	var dedupe *string
	if t.DedupeKey != "" {
		dedupe = &t.DedupeKey
	}
	err = db.QueryRow(ctx, `
		INSERT INTO task (type, dedupe_key, priority, run_after, max_attempts, inputs, opportunity_id, question_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING
		RETURNING id`,
		t.Type, dedupe, t.Priority, runAfter, t.MaxAttempts, inputs, t.OpportunityID, t.QuestionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = db.QueryRow(ctx, `SELECT id FROM task WHERE dedupe_key = $1`, t.DedupeKey).Scan(&id)
		return id, false, err
	}
	return id, err == nil, err
}

// Claim leases the highest-priority runnable task whose type is in types
// (all types if empty). A running task whose lease expired is reclaimed if it
// has attempts left, or moved to dead if not. Returns nil, nil when idle.
func Claim(ctx context.Context, db DB, types []string, lease time.Duration) (*Task, error) {
	if _, err := db.Exec(ctx, `
		UPDATE task SET state = 'dead', finished_at = now(),
			last_error = coalesce(last_error || '; ', '') || 'lease expired'
		WHERE state = 'running' AND lease_until < now() AND attempts >= max_attempts`); err != nil {
		return nil, fmt.Errorf("reap expired leases: %w", err)
	}
	var t Task
	err := db.QueryRow(ctx, `
		UPDATE task SET state = 'running', attempts = attempts + 1,
			lease_until = now() + make_interval(secs => $2), started_at = now()
		WHERE id = (
			SELECT id FROM task
			WHERE ((state = 'queued' AND run_after <= now()) OR (state = 'running' AND lease_until < now()))
			  AND (coalesce(cardinality($1::text[]), 0) = 0 OR type = ANY($1))
			ORDER BY priority DESC, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, type, dedupe_key, priority, attempts, max_attempts, inputs, opportunity_id, question_id`,
		types, lease.Seconds()).Scan(&t.ID, &t.Type, &t.DedupeKey, &t.Priority, &t.Attempts, &t.MaxAttempts,
		&t.Inputs, &t.OpportunityID, &t.QuestionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Heartbeat extends the lease of a running task.
func Heartbeat(ctx context.Context, db DB, id int64, lease time.Duration) error {
	_, err := db.Exec(ctx, `UPDATE task SET lease_until = now() + make_interval(secs => $2)
		WHERE id = $1 AND state = 'running'`, id, lease.Seconds())
	return err
}

// Complete marks a task done with its outputs.
func Complete(ctx context.Context, db DB, id int64, outputs any) error {
	out, err := json.Marshal(outputs)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `UPDATE task SET state = 'done', outputs = $2, finished_at = now(), lease_until = NULL
		WHERE id = $1`, id, out)
	return err
}

// permanentError marks a failure that must not be retried.
type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent wraps err so Fail sends the task straight to dead.
func Permanent(err error) error { return permanentError{err} }

// IsPermanent reports whether err was wrapped with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Backoff returns the retry delay after the given attempt: 1m, 4m, 16m, …
// capped at one hour.
func Backoff(attempt int) time.Duration {
	d := time.Minute
	for i := 1; i < attempt && d < time.Hour; i++ {
		d *= 4
	}
	return min(d, time.Hour)
}

// Fail records an error. The task is re-queued after Backoff(attempts) if it
// has attempts left and the error isn't permanent; otherwise it goes to dead.
// It returns true if the task was dead-lettered.
func Fail(ctx context.Context, db DB, t *Task, cause error) (dead bool, err error) {
	dead = IsPermanent(cause) || t.Attempts >= t.MaxAttempts
	if dead {
		_, err = db.Exec(ctx, `UPDATE task SET state = 'dead', last_error = $2, finished_at = now(), lease_until = NULL
			WHERE id = $1`, t.ID, cause.Error())
		return true, err
	}
	_, err = db.Exec(ctx, `UPDATE task SET state = 'queued', last_error = $2, lease_until = NULL,
		run_after = now() + make_interval(secs => $3)
		WHERE id = $1`, t.ID, cause.Error(), Backoff(t.Attempts).Seconds())
	return false, err
}

// Defer puts a running task back in the queue without spending an attempt,
// e.g. when a capacity cap is reached.
func Defer(ctx context.Context, db DB, t *Task, until time.Time, reason string) error {
	_, err := db.Exec(ctx, `UPDATE task SET state = 'queued', attempts = greatest(attempts - 1, 0),
		lease_until = NULL, run_after = $2, last_error = $3 WHERE id = $1`, t.ID, until, reason)
	return err
}

// Stat is a count of tasks by type and state.
type Stat struct {
	Type  string
	State string
	Count int
}

// Stats summarises the queue.
func Stats(ctx context.Context, db DB) ([]Stat, error) {
	rows, err := db.Query(ctx, `SELECT type, state, count(*) FROM task GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Stat, error) {
		var s Stat
		err := r.Scan(&s.Type, &s.State, &s.Count)
		return s, err
	})
}

// DeadTask is a dead-lettered task for reporting.
type DeadTask struct {
	ID         int64
	Type       string
	Attempts   int
	LastError  string
	FinishedAt time.Time
}

// DeadLetters lists the most recent dead tasks.
func DeadLetters(ctx context.Context, db DB, limit int) ([]DeadTask, error) {
	rows, err := db.Query(ctx, `SELECT id, type, attempts, coalesce(last_error, ''), coalesce(finished_at, created_at)
		FROM task WHERE state = 'dead' ORDER BY finished_at DESC NULLS LAST LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeadTask, error) {
		var d DeadTask
		err := r.Scan(&d.ID, &d.Type, &d.Attempts, &d.LastError, &d.FinishedAt)
		return d, err
	})
}
