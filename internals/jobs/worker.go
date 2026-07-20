package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
	"recallo/internals/logger"
)

const (
	brpopTimeout  = 2 * time.Second   // max block time per BRPOP call
	schedulerTick = 5 * time.Second   // how often delayed ZSET is drained
	maxBackoffSec = 512.0             // cap: 2^9 = 512s ≈ 8.5 min
)

type HandlerFunc func(ctx context.Context, job Job) error

// WorkerPool manages bounded goroutine pools per job type.
type WorkerPool struct {
	db       *sql.DB
	rdb      *redis.Client
	handlers map[JobType]HandlerFunc
}

// NewWorkerPool constructs the pool. Register handlers before calling Start.
func NewWorkerPool(db *sql.DB, rdb *redis.Client) *WorkerPool {
	return &WorkerPool{
		db:       db,
		rdb:      rdb,
		handlers: make(map[JobType]HandlerFunc),
	}
}

// Register wires a handler for a job type.
// Must be called before Start.
func (p *WorkerPool) Register(jobType JobType, h HandlerFunc) {
	p.handlers[jobType] = h
}

// Start launches worker goroutines. concurrency maps JobType → goroutine count.
// Blocks until ctx is cancelled.
// Typical call from main.go:
//
//	pool.Register(jobs.TypeTranscribe, transcriptSvc.Handle)
//	pool.Register(jobs.TypeSummarize,  summarySvc.Handle)
//	go pool.Start(ctx, map[jobs.JobType]int{
//	    jobs.TypeTranscribe: 3,
//	    jobs.TypeSummarize:  2,
//	})
func (p *WorkerPool) Start(ctx context.Context, concurrency map[JobType]int) {
	for jobType, n := range concurrency {
		for i := range n {
			go p.workerLoop(ctx, jobType, i)
		}
	}
	// Single delayed-job scheduler shared across all types.
	go p.delayedScheduler(ctx)

	// Block until context cancelled (caller's goroutine waits here).
	<-ctx.Done()
	logger.App.Printf("[jobs] context cancelled — worker pool draining")
}

// workerLoop is the hot path: blocks on BRPOP, dispatches, applies retry logic.
func (p *WorkerPool) workerLoop(ctx context.Context, jobType JobType, slot int) {
	key := RedisQueueKey(jobType)
	logger.App.Printf("[jobs] worker started type=%s slot=%d", jobType, slot)

	for {
		select {
		case <-ctx.Done():
			logger.App.Printf("[jobs] worker stopping type=%s slot=%d", jobType, slot)
			return
		default:
		}

		res, err := p.rdb.BRPop(ctx, brpopTimeout, key).Result()
		if err == redis.Nil || err == context.Canceled || err == context.DeadlineExceeded {
			continue
		}
		if err != nil {
			logger.App.Printf("[jobs] brpop error type=%s slot=%d err=%v", jobType, slot, err)
			continue
		}

		var job Job
		if err := json.Unmarshal([]byte(res[1]), &job); err != nil {
			logger.App.Printf("[jobs] unmarshal error type=%s data=%q err=%v", jobType, res[1], err)
			continue
		}

		p.process(ctx, job)
	}
}

// process runs the handler and applies success/failure transitions.
func (p *WorkerPool) process(ctx context.Context, job Job) {
	handler, ok := p.handlers[job.Type]
	if !ok {
		logger.App.Printf("[jobs] no handler registered for type=%s id=%s", job.Type, job.ID)
		return
	}

	// Mark running in Postgres so the reconciler skips it.
	p.setStatus(job.ID, "running", 0, time.Time{})

	if err := handler(ctx, job); err != nil {
		logger.App.Printf("[jobs] handler error type=%s id=%s attempt=%d err=%v", job.Type, job.ID, job.Attempts+1, err)
		p.handleFailure(context.Background(), job)
		return
	}

	p.setStatus(job.ID, "completed", job.Attempts, time.Time{})
	logger.App.Printf("[jobs] completed type=%s id=%s", job.Type, job.ID)
}

// handleFailure applies exponential backoff or promotes to DLQ.
// Backoff formula: min(2^attempts * 2s, maxBackoffSec)
// attempts 0→2s, 1→4s, 2→8s, 3→16s, 4→32s, 5→DLQ
func (p *WorkerPool) handleFailure(ctx context.Context, job Job) {
	job.Attempts++
	if job.Attempts >= job.MaxRetries {
		p.moveToDLQ(ctx, job)
		return
	}

	backoffSec := math.Min(math.Pow(2, float64(job.Attempts))*2, maxBackoffSec)
	job.NextRunAt = time.Now().UTC().Add(time.Duration(backoffSec) * time.Second)
	job.Status = "pending"

	jobBytes, _ := json.Marshal(job)
	score := float64(job.NextRunAt.Unix())
	if err := p.rdb.ZAdd(ctx, RedisDelayedKey, redis.Z{Score: score, Member: string(jobBytes)}).Err(); err != nil {
		logger.App.Printf("[jobs] zadd delayed error id=%s err=%v", job.ID, err)
	}
	p.setStatus(job.ID, "pending", job.Attempts, job.NextRunAt)
	logger.App.Printf("[jobs] retry scheduled id=%s attempt=%d next_run_at=%s", job.ID, job.Attempts, job.NextRunAt.Format(time.RFC3339))
}

// moveToDLQ pushes to the DLQ LIST and marks Postgres failed.
func (p *WorkerPool) moveToDLQ(ctx context.Context, job Job) {
	job.Status = "failed"
	jobBytes, _ := json.Marshal(job)
	if err := p.rdb.LPush(ctx, RedisDLQKey, jobBytes).Err(); err != nil {
		logger.App.Printf("[jobs] dlq lpush error id=%s err=%v", job.ID, err)
	}
	p.setStatus(job.ID, "failed", job.Attempts, time.Time{})
	logger.App.Printf("[jobs] DLQ id=%s type=%s after %d attempts", job.ID, job.Type, job.Attempts)
}

// delayedScheduler polls the ZSET every schedulerTick, moving ready jobs
// back to their ready LIST. This is the retry re-queue path.
func (p *WorkerPool) delayedScheduler(ctx context.Context) {
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	logger.App.Printf("[jobs] delayed scheduler started tick=%s", schedulerTick)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.drainDelayed(ctx)
		}
	}
}

// drainDelayed moves all ZSET members with score <= now() to their ready queues.
func (p *WorkerPool) drainDelayed(ctx context.Context) {
	now := float64(time.Now().Unix())
	members, err := p.rdb.ZRangeByScore(ctx, RedisDelayedKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: fmt.Sprintf("%f", now),
	}).Result()
	if err != nil || len(members) == 0 {
		return
	}

	for _, member := range members {
		var job Job
		if err := json.Unmarshal([]byte(member), &job); err != nil {
			logger.App.Printf("[jobs] delayed: unmarshal error err=%v", err)
			continue
		}
		// Atomic: remove from ZSET, push to ready LIST.
		removed, err := p.rdb.ZRem(ctx, RedisDelayedKey, member).Result()
		if err != nil || removed == 0 {
			continue // another instance may have already claimed it
		}
		jobBytes, _ := json.Marshal(job)
		p.rdb.LPush(ctx, RedisQueueKey(job.Type), jobBytes) //nolint:errcheck
		logger.App.Printf("[jobs] delayed→ready id=%s type=%s", job.ID, job.Type)
	}
}

// setStatus updates the Postgres job_queue row. next_run_at is only updated
// when non-zero (retry scheduling).
func (p *WorkerPool) setStatus(id, status string, attempts int, nextRunAt time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if nextRunAt.IsZero() {
		_, err := p.db.ExecContext(ctx,
			`UPDATE job_queue SET status=$1, attempts=$2 WHERE id=$3`,
			status, attempts, id)
		if err != nil {
			logger.App.Printf("[jobs] setStatus error id=%s err=%v", id, err)
		}
		return
	}
	_, err := p.db.ExecContext(ctx,
		`UPDATE job_queue SET status=$1, attempts=$2, next_run_at=$3 WHERE id=$4`,
		status, attempts, nextRunAt, id)
	if err != nil {
		logger.App.Printf("[jobs] setStatus error id=%s err=%v", id, err)
	}
}
