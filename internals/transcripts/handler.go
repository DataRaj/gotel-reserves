package transcripts

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type transcriptResponse struct {
	ID              int64           `json:"id"`
	RoomLivekitName string          `json:"room_livekit_name"`
	RecordingID     int64           `json:"recording_id"`
	EgressID        string          `json:"egress_id"`
	Text            string          `json:"text"`
	WordsJSON       json.RawMessage `json:"words_json"`
	Confidence      float64         `json:"confidence"`
	DurationSec     int             `json:"duration_sec"`
	Model           string          `json:"model"`
	Language        string          `json:"language"`
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

	var row transcriptResponse
	var wordsRaw []byte
	var confidence sql.NullFloat64
	var durationSec sql.NullInt32

	err := h.db.QueryRowContext(r.Context(), `
		SELECT id, room_livekit_name, recording_id, egress_id, text, words_json,
		       COALESCE(confidence, 0), COALESCE(duration_sec, 0), model, language, created_at
		FROM transcripts
		WHERE room_livekit_name = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, roomName).Scan(
		&row.ID, &row.RoomLivekitName, &row.RecordingID, &row.EgressID,
		&row.Text, &wordsRaw, &confidence, &durationSec,
		&row.Model, &row.Language, &row.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, `{"error":"transcript not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	row.Confidence = confidence.Float64
	row.DurationSec = int(durationSec.Int32)
	row.WordsJSON = json.RawMessage(wordsRaw)

	writeJSON(w, row)
}

func (h *Handler) HandleGetByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	var row transcriptResponse
	var wordsRaw []byte
	var confidence sql.NullFloat64
	var durationSec sql.NullInt32

	err = h.db.QueryRowContext(r.Context(), `
		SELECT id, room_livekit_name, recording_id, egress_id, text, words_json,
		       COALESCE(confidence, 0), COALESCE(duration_sec, 0), model, language, created_at
		FROM transcripts
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.RoomLivekitName, &row.RecordingID, &row.EgressID,
		&row.Text, &wordsRaw, &confidence, &durationSec,
		&row.Model, &row.Language, &row.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, `{"error":"transcript not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	row.Confidence = confidence.Float64
	row.DurationSec = int(durationSec.Int32)
	row.WordsJSON = json.RawMessage(wordsRaw)

	writeJSON(w, row)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, `{"error":"encode error"}`, http.StatusInternalServerError)
	}
}
