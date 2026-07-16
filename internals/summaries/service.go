// Package summaries — service.go
package summaries

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"recallo/db"
	"recallo/internals/configs"
	"recallo/internals/jobs"
	"recallo/internals/logger"
)

// ── transcriptRow ─────────────────────────────────────────────────────────────

type transcriptRow struct {
	id              int64
	roomLivekitName string
	text            string
}

// ── Service ───────────────────────────────────────────────────────────────────

// Service orchestrates the AI summarisation pipeline.
// Dependencies injected at construction — no package-level state.
type Service struct {
	db    *sql.DB
	grok  *grokClient
	model string
}

// NewService constructs the summaries Service.
func NewService(database *sql.DB, cfg configs.GrokConfig) *Service {
	return &Service{
		db:    database,
		grok:  newGrokClient(cfg),
		model: cfg.Model,
	}
}

// Handle is the jobs.HandlerFunc for TypeSummarize jobs.
// Return nil  → job marked completed by the worker pool.
// Return err  → worker pool applies backoff / DLQ after max_retries.
func (s *Service) Handle(ctx context.Context, job jobs.Job) error {
	var payload jobs.SummarizePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		// Malformed payload: can never succeed. Log and swallow — avoid infinite retry.
		logger.App.Printf("[summaries] malformed payload job_id=%s err=%v — skipping", job.ID, err)
		return nil
	}

	transcriptID, err := strconv.ParseInt(payload.TranscriptID, 10, 64)
	if err != nil {
		logger.App.Printf("[summaries] invalid transcript_id=%q job_id=%s — skipping", payload.TranscriptID, job.ID)
		return nil
	}

	logger.App.Printf("[summaries] starting job_id=%s transcript_id=%d room=%s category=%s",
		job.ID, transcriptID, payload.RoomLivekitName, payload.Category)

	// 1. Fetch transcript text from Postgres.
	tr, err := s.fetchTranscript(ctx, transcriptID)
	if err != nil {
		return fmt.Errorf("summaries.Handle: fetch transcript: %w", err)
	}

	if tr.text == "" {
		// Empty transcript — no meaningful summary possible. Mark complete.
		logger.App.Printf("[summaries] empty transcript, skipping summarisation job_id=%s", job.ID)
		return nil
	}

	// 2. Select system prompt from category.
	systemPrompt := SystemPromptFor(RoomCategory(payload.Category))

	// 3. Call Grok (automatically chunks long transcripts).
	output, err := s.grok.Summarize(ctx, systemPrompt, tr.text)
	if err != nil {
		if IsRateLimit(err) {
			logger.App.Printf("[summaries] grok rate limited job_id=%s — %v", job.ID, err)
		} else {
			logger.App.Printf("[summaries] grok error job_id=%s err=%v", job.ID, err)
		}
		return err // worker pool applies exponential backoff
	}

	// 4. Persist summary atomically.
	if err := s.persistSummary(ctx, tr, payload, output); err != nil {
		return fmt.Errorf("summaries.Handle: persist: %w", err)
	}

	logger.App.Printf("[summaries] completed job_id=%s transcript_id=%d tags=%v",
		job.ID, transcriptID, output.DiscussionTags)
	return nil
}

// persistSummary writes the summary row inside a transaction.
// ON CONFLICT (transcript_id) DO UPDATE makes the write idempotent on retry.
func (s *Service) persistSummary(
	ctx context.Context,
	tr *transcriptRow,
	payload jobs.SummarizePayload,
	out *SummaryOutput,
) error {
	keyPointsJSON, err := json.Marshal(out.KeyPoints)
	if err != nil {
		return fmt.Errorf("persistSummary: marshal key_points: %w", err)
	}
	actionItemsJSON, err := json.Marshal(out.ActionItems)
	if err != nil {
		return fmt.Errorf("persistSummary: marshal action_items: %w", err)
	}
	decisionsMadeJSON, err := json.Marshal(out.DecisionsMade)
	if err != nil {
		return fmt.Errorf("persistSummary: marshal decisions_made: %w", err)
	}
	discussionTagsJSON, err := json.Marshal(out.DiscussionTags)
	if err != nil {
		return fmt.Errorf("persistSummary: marshal discussion_tags: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("persistSummary: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx, `
		INSERT INTO summaries
		    (transcript_id, room_livekit_name, category,
		     executive_summary, key_points, action_items,
		     decisions_made, discussion_tags, model, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (transcript_id) DO UPDATE
		    SET executive_summary = EXCLUDED.executive_summary,
		        key_points        = EXCLUDED.key_points,
		        action_items      = EXCLUDED.action_items,
		        decisions_made    = EXCLUDED.decisions_made,
		        discussion_tags   = EXCLUDED.discussion_tags,
		        model             = EXCLUDED.model
	`,
		tr.id,
		tr.roomLivekitName,
		string(payload.Category),
		out.ExecutiveSummary,
		keyPointsJSON,
		actionItemsJSON,
		decisionsMadeJSON,
		discussionTagsJSON,
		s.model, // stored for auditing which Grok model generated this summary
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("persistSummary: upsert summaries: %w", err)
	}

	return tx.Commit()
}

// fetchTranscript loads the transcript text and room name by primary key.
func (s *Service) fetchTranscript(ctx context.Context, transcriptID int64) (*transcriptRow, error) {
	var tr transcriptRow
	err := db.DB.QueryRowContext(ctx, `
		SELECT id, room_livekit_name, text
		FROM transcripts
		WHERE id = $1
	`, transcriptID).Scan(&tr.id, &tr.roomLivekitName, &tr.text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("summaries: transcript not found id=%d", transcriptID)
	}
	if err != nil {
		return nil, fmt.Errorf("summaries: query transcript: %w", err)
	}
	return &tr, nil
}
