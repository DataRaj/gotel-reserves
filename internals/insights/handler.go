// Package insights exposes a single, always-200 status endpoint that reports the
// state of a room's post-meeting pipeline (egress → Deepgram transcript → Grok
// summary) and embeds the transcript/summary once each becomes available.
//
// Why it exists: the transcript and summary endpoints return 404 while their async
// jobs are still running, which makes a polling frontend throw. This endpoint gives
// the UI one place to poll that never 404s for an in-progress pipeline — it returns
// a "processing" payload with the current step until everything is ready.
package insights

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Handler serves GET /api/v1/rooms/{room_name}/insights.
type Handler struct {
	db *sql.DB
}

func NewHandler(db *sql.DB) *Handler {
	return &Handler{db: db}
}

// pipeline status values.
const (
	statusLive       = "live"       // meeting still in progress
	statusProcessing = "processing" // pipeline running
	statusCompleted  = "completed"  // summary ready
	statusFailed     = "failed"     // a pipeline job failed
)

// pipeline step values (finer-grained than status).
const (
	stepLive         = "live"
	stepFinalizing   = "finalizing" // room ended, awaiting egress
	stepRecording    = "recording"  // egress in progress
	stepTranscribing = "transcribing"
	stepSummarizing  = "summarizing"
	stepCompleted    = "completed"
	stepFailed       = "failed"
)

type insightsResponse struct {
	RoomLivekitName string             `json:"room_livekit_name"`
	Status          string             `json:"status"`
	Step            string             `json:"step"`
	Message         string             `json:"message"`
	Transcript      *transcriptPayload `json:"transcript"`
	Summary         *summaryPayload    `json:"summary"`
}

type transcriptPayload struct {
	ID          int64           `json:"id"`
	Text        string          `json:"text"`
	WordsJSON   json.RawMessage `json:"words_json"`
	Confidence  float64         `json:"confidence"`
	DurationSec int             `json:"duration_sec"`
	Model       string          `json:"model"`
	Language    string          `json:"language"`
	CreatedAt   time.Time       `json:"created_at"`
}

type summaryPayload struct {
	ID               int64           `json:"id"`
	TranscriptID     int64           `json:"transcript_id"`
	Category         string          `json:"category"`
	ExecutiveSummary string          `json:"executive_summary"`
	KeyPoints        json.RawMessage `json:"key_points"`
	ActionItems      json.RawMessage `json:"action_items"`
	DecisionsMade    json.RawMessage `json:"decisions_made"`
	DiscussionTags   json.RawMessage `json:"discussion_tags"`
	Model            string          `json:"model"`
	CreatedAt        time.Time       `json:"created_at"`
}

// HandleGetByRoomName — GET /api/v1/rooms/{room_name}/insights
func (h *Handler) HandleGetByRoomName(w http.ResponseWriter, r *http.Request) {
	roomName := r.PathValue("room_name")
	if roomName == "" {
		writeJSONError(w, http.StatusBadRequest, "room_name is required")
		return
	}

	// Derive pipeline state from the latest recording/transcript/summary for the room.
	var roomStatus string
	var recStatus sql.NullString
	var transcriptID sql.NullInt64
	var summaryID sql.NullInt64

	err := h.db.QueryRowContext(r.Context(), `
		SELECT r.status,
		       (SELECT status FROM recordings  WHERE room_livekit_name = r.livekit_room_name ORDER BY created_at DESC LIMIT 1),
		       (SELECT id     FROM transcripts WHERE room_livekit_name = r.livekit_room_name ORDER BY created_at DESC LIMIT 1),
		       (SELECT id     FROM summaries   WHERE room_livekit_name = r.livekit_room_name ORDER BY created_at DESC LIMIT 1)
		FROM rooms r
		WHERE r.livekit_room_name = $1
	`, roomName).Scan(&roomStatus, &recStatus, &transcriptID, &summaryID)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "room not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	resp := insightsResponse{RoomLivekitName: roomName}

	switch {
	case summaryID.Valid:
		resp.Status, resp.Step = statusCompleted, stepCompleted
		resp.Message = "Insights ready."
	case transcriptID.Valid:
		resp.Status, resp.Step = statusProcessing, stepSummarizing
		resp.Message = "Synthesizing the executive summary and action items…"
	case recStatus.Valid && recStatus.String == "completed":
		resp.Status, resp.Step = statusProcessing, stepTranscribing
		resp.Message = "Deepgram is generating word-level timestamps…"
	case recStatus.Valid && recStatus.String == "recording":
		resp.Status, resp.Step = statusProcessing, stepRecording
		resp.Message = "Finalizing the recording…"
	case recStatus.Valid && recStatus.String == "failed":
		resp.Status, resp.Step = statusFailed, stepFailed
		resp.Message = "Recording failed to process."
	case roomStatus == "ended":
		resp.Status, resp.Step = statusProcessing, stepFinalizing
		resp.Message = "Wrapping up the recording…"
	default: // draft / live
		resp.Status, resp.Step = statusLive, stepLive
		resp.Message = "Meeting in progress."
	}

	// Attach the transcript as soon as it exists (available during summarizing too).
	if transcriptID.Valid {
		if t, err := h.loadTranscript(r, transcriptID.Int64); err == nil {
			resp.Transcript = t
		}
	}
	// Attach the summary once completed.
	if summaryID.Valid {
		if s, err := h.loadSummary(r, summaryID.Int64); err == nil {
			resp.Summary = s
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) loadTranscript(r *http.Request, id int64) (*transcriptPayload, error) {
	var t transcriptPayload
	var words []byte
	var confidence sql.NullFloat64
	var durationSec sql.NullInt32
	err := h.db.QueryRowContext(r.Context(), `
		SELECT id, text, words_json, COALESCE(confidence, 0), COALESCE(duration_sec, 0), model, language, created_at
		FROM transcripts WHERE id = $1
	`, id).Scan(&t.ID, &t.Text, &words, &confidence, &durationSec, &t.Model, &t.Language, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.Confidence = confidence.Float64
	t.DurationSec = int(durationSec.Int32)
	t.WordsJSON = json.RawMessage(words)
	return &t, nil
}

func (h *Handler) loadSummary(r *http.Request, id int64) (*summaryPayload, error) {
	var s summaryPayload
	var kp, ai, dm, dt []byte
	err := h.db.QueryRowContext(r.Context(), `
		SELECT id, transcript_id, category, executive_summary,
		       key_points, action_items, decisions_made, discussion_tags, model, created_at
		FROM summaries WHERE id = $1
	`, id).Scan(&s.ID, &s.TranscriptID, &s.Category, &s.ExecutiveSummary,
		&kp, &ai, &dm, &dt, &s.Model, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	s.KeyPoints = json.RawMessage(kp)
	s.ActionItems = json.RawMessage(ai)
	s.DecisionsMade = json.RawMessage(dm)
	s.DiscussionTags = json.RawMessage(dt)
	return &s, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
