package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	lkauth "github.com/livekit/protocol/auth"
	lkproto "github.com/livekit/protocol/livekit"
	lkwebhook "github.com/livekit/protocol/webhook"

	"recallo/db"
	"recallo/internals/jobs"
	"recallo/internals/logger"
)

type Handler struct {
	// keyProvider maps API key → secret for HMAC validation.
	keyProvider lkauth.KeyProvider
	// jobClient enqueues async pipeline jobs after egress events.
	jobClient jobs.Client
}

// NewHandler constructs the webhook handler.
// apiKey and webhookSecret come from configs.LiveKitConfig.
// jobClient is passed explicitly — no package-level singletons.
func NewHandler(apiKey, webhookSecret string, jobClient jobs.Client) *Handler {
	return &Handler{
		keyProvider: lkauth.NewSimpleKeyProvider(apiKey, webhookSecret),
		jobClient:   jobClient,
	}
}

// RegisterRoutes wires the webhook endpoint into the mux.
// No auth middleware — LiveKit authenticates via HMAC, not our JWT.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/livekit", h.handleWebhook)
}

// handleWebhook is the single entry point for all inbound LiveKit webhook events.
func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// 1. Validate signature + parse event.
	event, err := lkwebhook.ReceiveWebhookEvent(r, h.keyProvider)
	if err != nil {
		logger.App.Printf("[webhooks] signature/parse error: %v", err)
		http.Error(w, "invalid webhook signature or payload", http.StatusUnauthorized)
		return
	}

	eventID := event.GetId()
	eventType := event.GetEvent()

	// 2. Deduplication.
	rawPayload, _ := json.Marshal(event)
	inserted, err := insertWebhookEvent(r.Context(), eventID, eventType, rawPayload)
	if err != nil {
		logger.App.Printf("[webhooks] db error storing event %s: %v", eventID, err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if !inserted {
		logger.App.Printf("[webhooks] duplicate event ignored: id=%s type=%s", eventID, eventType)
		w.WriteHeader(http.StatusOK)
		return
	}

	// 3. Dispatch.
	logger.App.Printf("[webhooks] processing event: id=%s type=%s", eventID, eventType)
	if err := h.dispatch(r.Context(), eventType, event); err != nil {
		// Log but return 200: deduped, retry won't help. Reconciler corrects divergence.
		logger.App.Printf("[webhooks] dispatch error for %s id=%s: %v", eventType, eventID, err)
	}

	w.WriteHeader(http.StatusOK)
}

// dispatch routes the parsed event to the appropriate handler.
func (h *Handler) dispatch(ctx context.Context, eventType string, event *lkproto.WebhookEvent) error {
	switch eventType {
	case "room_started":
		return handleRoomStarted(ctx, event)
	case "room_finished":
		return handleRoomFinished(ctx, event)
	case "participant_joined":
		return handleParticipantJoined(ctx, event)
	case "participant_left":
		return handleParticipantLeft(ctx, event)
	case "egress_started":
		return handleEgressStarted(ctx, event)
	case "egress_ended":
		return h.handleEgressEnded(ctx, event)
	default:
		logger.App.Printf("[webhooks] unhandled event type: %s", eventType)
		return nil
	}
}

// ── Event handlers ────────────────────────────────────────────────────────────

func handleRoomStarted(ctx context.Context, event *lkproto.WebhookEvent) error {
	if event.Room == nil {
		return fmt.Errorf("webhooks.handleRoomStarted: missing room in event")
	}
	_, err := db.DB.ExecContext(ctx, `
		UPDATE rooms
		SET status = 'live', started_at = $1
		WHERE livekit_room_name = $2
		  AND status = 'draft'
	`, time.Now().UTC(), event.Room.Name)
	if err != nil {
		return fmt.Errorf("webhooks.handleRoomStarted: db: %w", err)
	}
	logger.App.Printf("[webhooks] room_started: room=%s → status=live", event.Room.Name)
	return nil
}

func handleRoomFinished(ctx context.Context, event *lkproto.WebhookEvent) error {
	if event.Room == nil {
		return fmt.Errorf("webhooks.handleRoomFinished: missing room in event")
	}
	_, err := db.DB.ExecContext(ctx, `
		UPDATE rooms
		SET status = 'ended', ended_at = $1
		WHERE livekit_room_name = $2
		  AND status != 'ended'
	`, time.Now().UTC(), event.Room.Name)
	if err != nil {
		return fmt.Errorf("webhooks.handleRoomFinished: db: %w", err)
	}
	logger.App.Printf("[webhooks] room_finished: room=%s → status=ended", event.Room.Name)
	return nil
}

// handleParticipantJoined upserts a participant attendance record.
// ON CONFLICT DO UPDATE clears left_at if a left event arrived out-of-order.
func handleParticipantJoined(ctx context.Context, event *lkproto.WebhookEvent) error {
	if event.Room == nil || event.Participant == nil {
		return fmt.Errorf("webhooks.handleParticipantJoined: missing room or participant")
	}
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO room_participants (room_livekit_name, identity, display_name, joined_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (room_livekit_name, identity)
		DO UPDATE SET joined_at = EXCLUDED.joined_at, left_at = NULL
	`, event.Room.Name, event.Participant.Identity, event.Participant.Name, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("webhooks.handleParticipantJoined: db: %w", err)
	}
	logger.App.Printf("[webhooks] participant_joined: room=%s identity=%s", event.Room.Name, event.Participant.Identity)
	return nil
}

// handleParticipantLeft sets left_at. ON CONFLICT handles out-of-order delivery.
func handleParticipantLeft(ctx context.Context, event *lkproto.WebhookEvent) error {
	if event.Room == nil || event.Participant == nil {
		return fmt.Errorf("webhooks.handleParticipantLeft: missing room or participant")
	}
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO room_participants (room_livekit_name, identity, display_name, left_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (room_livekit_name, identity)
		DO UPDATE SET left_at = EXCLUDED.left_at
	`, event.Room.Name, event.Participant.Identity, event.Participant.Name, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("webhooks.handleParticipantLeft: db: %w", err)
	}
	logger.App.Printf("[webhooks] participant_left: room=%s identity=%s", event.Room.Name, event.Participant.Identity)
	return nil
}

// handleEgressStarted creates the recordings row with status='recording'.
// The row is updated to 'completed' when egress_ended arrives.
func handleEgressStarted(ctx context.Context, event *lkproto.WebhookEvent) error {
	ei := event.GetEgressInfo()
	if ei == nil {
		return fmt.Errorf("webhooks.handleEgressStarted: missing egress_info")
	}
	roomName := ei.GetRoomName()
	egressID := ei.GetEgressId()
	if roomName == "" || egressID == "" {
		return fmt.Errorf("webhooks.handleEgressStarted: empty room_name or egress_id")
	}

	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO recordings (room_livekit_name, egress_id, status, created_at)
		VALUES ($1, $2, 'recording', $3)
		ON CONFLICT (egress_id) DO NOTHING
	`, roomName, egressID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("webhooks.handleEgressStarted: db: %w", err)
	}
	logger.App.Printf("[webhooks] egress_started: room=%s egress_id=%s", roomName, egressID)
	return nil
}

// handleEgressEnded is the critical pipeline trigger.
//
// Sequence (atomic transaction):
//  1. Resolve file_url from EgressInfo (first file output).
//  2. UPDATE recordings: status='completed', file_url, file_size_bytes, duration_sec, completed_at.
//  3. INSERT job_queue row for TypeTranscribe via EnqueueTx (same tx).
//  4. Commit. Redis push happens inside EnqueueTx (best-effort, non-fatal).
//
// If the tx rolls back, both the recording update and the job row are absent —
// consistent state. The reconciler detects stuck 'recording' rows and re-queues.
func (h *Handler) handleEgressEnded(ctx context.Context, event *lkproto.WebhookEvent) error {
	ei := event.GetEgressInfo()
	if ei == nil {
		return fmt.Errorf("webhooks.handleEgressEnded: missing egress_info")
	}

	// Status 3 = EGRESS_COMPLETE in the LiveKit protobuf enum.
	// We do not process failed egress (status != 3) here; reconciler handles cleanup.
	if ei.GetStatus() != lkproto.EgressStatus_EGRESS_COMPLETE {
		logger.App.Printf("[webhooks] egress_ended non-complete: egress_id=%s status=%s", ei.GetEgressId(), ei.GetStatus())
		return nil
	}

	roomName := ei.GetRoomName()
	egressID := ei.GetEgressId()

	// Extract file metadata from the first file output.
	fileURL, fileSizeBytes, durationSec := extractEgressFileInfo(ei)

	tx, err := db.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("webhooks.handleEgressEnded: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// 1. Update recording row.
	_, err = tx.ExecContext(ctx, `
		UPDATE recordings
		SET status          = 'completed',
		    file_url        = $1,
		    file_size_bytes = $2,
		    duration_sec    = $3,
		    completed_at    = $4
		WHERE egress_id = $5
		  AND status    = 'recording'
	`, fileURL, fileSizeBytes, durationSec, time.Now().UTC(), egressID)
	if err != nil {
		return fmt.Errorf("webhooks.handleEgressEnded: update recording: %w", err)
	}

	// 2. Enqueue transcription job inside the same transaction.
	transcribePayload := jobs.TranscribePayload{
		RoomLivekitName: roomName,
		EgressID:        egressID,
		FileURL:         fileURL,
	}
	if err := h.jobClient.EnqueueTx(ctx, tx, jobs.TypeTranscribe, transcribePayload); err != nil {
		return fmt.Errorf("webhooks.handleEgressEnded: enqueue transcribe job: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("webhooks.handleEgressEnded: commit: %w", err)
	}

	logger.App.Printf("[webhooks] egress_ended: room=%s egress_id=%s file_url=%s size_bytes=%d duration_sec=%d → transcribe job enqueued",
		roomName, egressID, fileURL, fileSizeBytes, durationSec)
	return nil
}

func extractEgressFileInfo(ei *lkproto.EgressInfo) (fileURL string, fileSizeBytes int64, durationSec int) {
	// Direct file output (most common for RoomComposite egress).
	if fi := ei.GetFile(); fi != nil {
		return fi.GetLocation(), fi.GetSize(), int(fi.GetDuration() / int64(time.Second))
	}
	// Segment output — use the manifest location.
	if si := ei.GetSegments(); si != nil {
		return si.GetPlaylistLocation(), si.GetSize(), int(si.GetDuration() / int64(time.Second))
	}
	return "", 0, 0
}

var insertWebhookEvent = func(ctx context.Context, eventID, eventType string, payload []byte) (bool, error) {
	result, err := db.DB.ExecContext(ctx, `
		INSERT INTO webhook_events (event_id, event_type, payload, received_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (event_id) DO NOTHING
	`, eventID, eventType, payload)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
