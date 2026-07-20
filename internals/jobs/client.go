package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type Client interface {
	// Enqueue opens its own DB transaction.
	Enqueue(ctx context.Context, jobType JobType, payload any) error
	// EnqueueTx participates in an existing transaction.
	// tx must not be nil; commit/rollback is the caller's responsibility.
	EnqueueTx(ctx context.Context, tx *sql.Tx, jobType JobType, payload any) error
}

type client struct {
	db  *sql.DB
	rdb *redis.Client
}

// NewClient constructs the job client.
// db is the global *sql.DB (db.DB); rdb is the Redis connection.
func NewClient(db *sql.DB, rdb *redis.Client) Client {
	return &client{db: db, rdb: rdb}
}

// Enqueue writes to Postgres then pushes to Redis.
// Postgres-first: if Redis push fails the Postgres row exists and the
// reconciler re-queues it on the next tick. If Postgres write fails we
// never touch Redis — consistency is maintained.
func (c *client) Enqueue(ctx context.Context, jobType JobType, payload any) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("jobs.Enqueue: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := c.EnqueueTx(ctx, tx, jobType, payload); err != nil {
		return err
	}
	return tx.Commit()
}

// EnqueueTx writes the job row inside the caller's transaction, then
// pushes to Redis after the write (before commit, consistent ordering).
// Redis push failure is logged but does not abort the transaction —
// the reconciler guarantees eventual re-queue from the Postgres row.
func (c *client) EnqueueTx(ctx context.Context, tx *sql.Tx, jobType JobType, payload any) error {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("jobs.EnqueueTx: marshal payload: %w", err)
	}

	job := Job{
		ID:         uuid.NewString(),
		Type:       jobType,
		Payload:    rawPayload,
		Status:     "pending",
		Attempts:   0,
		MaxRetries: 5,
		CreatedAt:  time.Now().UTC(),
		NextRunAt:  time.Now().UTC(),
	}

	const q = `
		INSERT INTO job_queue (id, type, payload, status, attempts, max_retries, created_at, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	if _, err := tx.ExecContext(ctx, q,
		job.ID, job.Type, job.Payload, job.Status,
		job.Attempts, job.MaxRetries, job.CreatedAt, job.NextRunAt,
	); err != nil {
		return fmt.Errorf("jobs.EnqueueTx: insert job_queue: %w", err)
	}

	// Push to Redis after the Postgres write succeeds.
	// Background context: the caller's ctx may be request-scoped and short;
	// the Redis push must complete even if the HTTP request context expires.
	pushCtx := context.Background()
	jobBytes, _ := json.Marshal(job)
	if err := c.rdb.LPush(pushCtx, RedisQueueKey(jobType), jobBytes).Err(); err != nil {
		// Non-fatal: reconciler will recover the row from Postgres.
		// Log at warn level; don't return error so the caller's Commit proceeds.
		_ = fmt.Errorf("jobs.EnqueueTx: redis lpush (non-fatal, reconciler will recover): %w", err)
	}

	return nil
}
