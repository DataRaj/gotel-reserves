package transcripts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"recallo/db"
	"recallo/internals/configs"
	"recallo/internals/jobs"
	"recallo/internals/logger"
)

type recordingRow struct {
	id          int64
	fileURL     sql.NullString
	durationSec sql.NullInt32
	roomTier    string
}

// ── Service ───────────────────────────────────────────────────────────────────

// Service orchestrates the full transcription pipeline.
// Dependencies are injected at construction — no package-level globals.
type Service struct {
	db        *sql.DB
	deepgram  *deepgramClient
	presigner *spacesPresigner
	jobClient jobs.Client
}

// NewService constructs the transcript Service.
// Registered into the worker pool in main.go; the workerPool calls Handle.
func NewService(
	database *sql.DB,
	deepgramCfg configs.DeepgramConfig,
	spacesCfg configs.SpacesConfig,
	jobClient jobs.Client,
) *Service {
	return &Service{
		db:        database,
		deepgram:  newDeepgramClient(deepgramCfg),
		presigner: newSpacesPresigner(spacesCfg),
		jobClient: jobClient,
	}
}

// Handle is the jobs.HandlerFunc for TypeTranscribe jobs.
// Return nil → job marked completed by the worker pool.
// Return non-nil → worker pool applies backoff / DLQ logic.
//
// Error classification:
//   - *RateLimitError → transient, return as-is; worker pool will backoff.
//   - context.Canceled / context.DeadlineExceeded → transient.
//   - All others → transient by default; exhausting max_retries → DLQ.
func (s *Service) Handle(ctx context.Context, job jobs.Job) error {
	var payload jobs.TranscribePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		// Malformed payload can never succeed — return nil to avoid infinite retry.
		// The DLQ will not receive it; it just marks completed with a warning.
		logger.App.Printf("[transcripts] malformed payload job_id=%s err=%v — skipping", job.ID, err)
		return nil
	}

	logger.App.Printf("[transcripts] starting job_id=%s room=%s egress=%s",
		job.ID, payload.RoomLivekitName, payload.EgressID)

	// 1. Fetch recording metadata from Postgres.
	rec, err := s.fetchRecording(ctx, payload.EgressID)
	if err != nil {
		return fmt.Errorf("transcripts.Handle: fetch recording: %w", err)
	}

	// 2. Build the presigned URL.
	// file_url stored in recordings is the raw Spaces path (e.g. "recordings/egress-id/file.mp4").
	// We presign it here so the URL is fresh and duration-scoped.
	fileKey := extractSpacesKey(rec.fileURL.String)
	recordingDuration := time.Duration(rec.durationSec.Int32) * time.Second
	presignedURL, err := s.presigner.PresignGet(fileKey, recordingDuration)
	if err != nil {
		return fmt.Errorf("transcripts.Handle: presign: %w", err)
	}

	// 3. Call Deepgram (Deepgram fetches the file — no server-side download).
	parsed, err := s.deepgram.Transcribe(ctx, presignedURL)
	if err != nil {
		// RateLimitError triggers faster re-inspection of the retry-after header.
		if IsRateLimit(err) {
			logger.App.Printf("[transcripts] rate limited job_id=%s — %v", job.ID, err)
		} else {
			logger.App.Printf("[transcripts] deepgram error job_id=%s err=%v", job.ID, err)
		}
		return err // worker pool applies backoff
	}

	if parsed.Text == "" {
		// Empty transcript (silent audio, processing error). Mark completed so
		// the job doesn't loop forever — empty is a valid terminal state.
		logger.App.Printf("[transcripts] empty transcript job_id=%s egress=%s", job.ID, payload.EgressID)
		return s.persistTranscript(ctx, rec.id, rec.roomTier, payload, parsed)
	}

	// 4+5. Persist transcript and enqueue summarize job atomically.
	if err := s.persistTranscript(ctx, rec.id, rec.roomTier, payload, parsed); err != nil {
		return fmt.Errorf("transcripts.Handle: persist: %w", err)
	}

	logger.App.Printf("[transcripts] completed job_id=%s egress=%s words=%d duration=%ds confidence=%.4f",
		job.ID, payload.EgressID, len(parsed.Words), parsed.DurationSec, parsed.Confidence)
	return nil
}

// persistTranscript writes the transcript row and enqueues TypeSummarize
// inside a single transaction.
func (s *Service) persistTranscript(ctx context.Context, recordingID int64, roomTier string, payload jobs.TranscribePayload, parsed *ParsedTranscript) error {
	wordsJSON, err := json.Marshal(parsed.Words)
	if err != nil {
		return fmt.Errorf("persistTranscript: marshal words: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("persistTranscript: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// INSERT transcript row. ON CONFLICT DO NOTHING on egress_id makes the
	// write idempotent — safe to retry after a partial failure.
	var transcriptID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO transcripts
		    (room_livekit_name, recording_id, egress_id, text, words_json, confidence, duration_sec, model, language, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (egress_id) DO UPDATE
		    SET text = EXCLUDED.text  -- idempotent re-write on retry
		RETURNING id
	`,
		payload.RoomLivekitName,
		recordingID,
		payload.EgressID,
		parsed.Text,
		wordsJSON,
		parsed.Confidence,
		parsed.DurationSec,
		parsed.Model,
		parsed.Language,
		time.Now().UTC(),
	).Scan(&transcriptID)
	if err != nil {
		return fmt.Errorf("persistTranscript: insert transcript: %w", err)
	}

	// Enqueue TypeSummarize inside the same transaction.
	// If Commit fails, both the transcript row and the job row are absent —
	// consistent state. Next retry re-runs from step 1 (Deepgram call).
	summarizePayload := jobs.SummarizePayload{
		RoomLivekitName: payload.RoomLivekitName,
		TranscriptID:    fmt.Sprintf("%d", transcriptID),
		Category:        roomTier,
	}
	if err := s.jobClient.EnqueueTx(ctx, tx, jobs.TypeSummarize, summarizePayload); err != nil {
		return fmt.Errorf("persistTranscript: enqueue summarize: %w", err)
	}

	return tx.Commit()
}

func (s *Service) fetchRecording(ctx context.Context, egressID string) (*recordingRow, error) {
	var rec recordingRow
	err := db.DB.QueryRowContext(ctx, `
		SELECT r.id, r.file_url, r.duration_sec, COALESCE(rm.tier, 'guest')
		FROM   recordings r
		LEFT JOIN rooms rm ON rm.livekit_room_name = r.room_livekit_name
		WHERE  r.egress_id = $1
	`, egressID).Scan(&rec.id, &rec.fileURL, &rec.durationSec, &rec.roomTier)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("transcripts: recording not found for egress_id=%s", egressID)
	}
	if err != nil {
		return nil, fmt.Errorf("transcripts: query recording: %w", err)
	}
	return &rec, nil
}

// extractSpacesKey strips the full Spaces endpoint prefix from a stored file_url,
// returning only the object key relative to the bucket root.
//
// Stored format:   https://bucket.nyc3.digitaloceanspaces.com/recordings/egress-id/file.mp4
// Returned key:    recordings/egress-id/file.mp4
//
// If the URL is already a bare key (no scheme), it is returned as-is.
func extractSpacesKey(fileURL string) string {
	if fileURL == "" {
		return ""
	}
	// Find the third "/" (after scheme://host/) and return everything after it.
	schemeEnd := 0
	slashCount := 0
	for i, c := range fileURL {
		if c == '/' {
			slashCount++
			if slashCount == 3 {
				schemeEnd = i + 1
				break
			}
		}
	}
	if schemeEnd == 0 {
		return fileURL // bare key, no scheme present
	}
	return fileURL[schemeEnd:]
}
