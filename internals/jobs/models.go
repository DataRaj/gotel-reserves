package jobs

import (
	"encoding/json"
	"time"
)

type JobType string

const (
	TypeTranscribe JobType = "transcribe"
	TypeSummarize  JobType = "summarize"
)

// Job is the canonical envelope stored in Redis and mirrored in Postgres.
// json tags drive Redis serialisation; db tags drive sqlx (if adopted later).
type Job struct {
	ID         string          `json:"id"`
	Type       JobType         `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	Status     string          `json:"status"`
	Attempts   int             `json:"attempts"`
	MaxRetries int             `json:"max_retries"`
	CreatedAt  time.Time       `json:"created_at"`
	NextRunAt  time.Time       `json:"next_run_at"`
}

// RedisQueueKey returns the Redis LIST key for a given job type.
func RedisQueueKey(t JobType) string { return "queue:" + string(t) }

// RedisDelayedKey is the ZSET key holding jobs pending exponential backoff.
const RedisDelayedKey = "queue:delayed"

// RedisDLQKey is the LIST key for jobs that exhausted max_retries.
const RedisDLQKey = "queue:dlq"

// ── Typed payloads ────────────────────────────────────────────────────────────

// TranscribePayload is enqueued by handleEgressEnded when recording is complete.
type TranscribePayload struct {
	RoomLivekitName string `json:"room_livekit_name"`
	EgressID        string `json:"egress_id"`
	FileURL         string `json:"file_url"` // public or presigned Spaces URL
}

type SummarizePayload struct {
	RoomLivekitName string `json:"room_livekit_name"`
	TranscriptID    string `json:"transcript_id"`
	Category        string `json:"category"`
}
