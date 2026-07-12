package summaries

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type summaryResponse struct {
	ID              int64           `json:"id"`
	TranscriptID    int64           `json:"transcript_id"`
	RoomLivekitName string          `json:"room_livekit_name"`
	Category        string          `json:"category"`
	ExecutiveSummary string         `json:"executive_summary"`
	KeyPoints       json.RawMessage `json:"key_points"`
	ActionItems     json.RawMessage `json:"action_items"`
	DecisionsMade   json.RawMessage `json:"decisions_made"`
	DiscussionTags  json.RawMessage `json:"discussion_tags"`
	Model           string          `json:"model"`
	CreatedAt       time.Time       `json:"created_at"`
}

type Handler struct {
	db *sql.DB
}

func NewHandler(db *sql.DB) *Handler {
	return &Handler{db: db}
}

func (h *Handler) HandleGetByRoomName(w http.ResponseWriter, r *http.Request) {
	roomName := r.PathValue("room_name")
	if roomName == "" {
		http.Error(w, `{"error":"room_name is required"}`, http.StatusBadRequest)
		return
	}

	row, err := h.querySummary(r, `
		SELECT id, transcript_id, room_livekit_name, category, executive_summary,
		       key_points, action_items, decisions_made, discussion_tags, model, created_at
		FROM summaries
		WHERE room_livekit_name = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, roomName)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, `{"error":"summary not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, row)
}

func (h *Handler) HandleGetByTranscriptID(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	row, err := h.querySummary(r, `
		SELECT id, transcript_id, room_livekit_name, category, executive_summary,
		       key_points, action_items, decisions_made, discussion_tags, model, created_at
		FROM summaries
		WHERE transcript_id = $1
	`, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, `{"error":"summary not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, row)
}

func (h *Handler) HandleGetByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	row, err := h.querySummary(r, `
		SELECT id, transcript_id, room_livekit_name, category, executive_summary,
		       key_points, action_items, decisions_made, discussion_tags, model, created_at
		FROM summaries
		WHERE id = $1
	`, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, `{"error":"summary not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, row)
}

func (h *Handler) querySummary(r *http.Request, query string, args ...any) (*summaryResponse, error) {
	var row summaryResponse
	var kp, ai, dm, dt []byte
	err := h.db.QueryRowContext(r.Context(), query, args...).Scan(
		&row.ID, &row.TranscriptID, &row.RoomLivekitName, &row.Category,
		&row.ExecutiveSummary, &kp, &ai, &dm, &dt, &row.Model, &row.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	row.KeyPoints = json.RawMessage(kp)
	row.ActionItems = json.RawMessage(ai)
	row.DecisionsMade = json.RawMessage(dm)
	row.DiscussionTags = json.RawMessage(dt)
	return &row, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, `{"error":"encode error"}`, http.StatusInternalServerError)
	}
}
