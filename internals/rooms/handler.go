package rooms

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Handler exposes the rooms domain over HTTP.
//
// Dependency: only depends on *Service (this package) — not on the livekit package.
// The handler layer's job is: parse request → call service → map error to status → encode response.
// No business logic lives here; no DB calls; no LiveKit calls.
type Handler struct {
	svc *Service
}

// NewHandler constructs the rooms HTTP handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes wires the rooms endpoints into a ServeMux.
// Called from the top-level routes.RegisterRoutes.
//
// Routes:
//
//	POST   /api/v1/rooms                    → CreateGuestRoom   (public — guest tier)
//	GET    /api/v1/rooms/{id}               → GetRoom
//	DELETE /api/v1/rooms/{id}               → EndRoom           (host only)
//	GET    /api/v1/rooms/{id}/token         → IssueGuestToken
//	POST   /api/v1/rooms/{id}/extend        → ExtendGuestSession (guest host only)
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/rooms", h.handleCreateGuestRoom)
	mux.HandleFunc("GET /api/v1/rooms", h.handleListRooms)
	mux.HandleFunc("GET /api/v1/rooms/{id}", h.handleGetRoom)
	mux.HandleFunc("DELETE /api/v1/rooms/{id}", h.handleEndRoom)
	mux.HandleFunc("GET /api/v1/rooms/{id}/token", h.handleIssueGuestToken)
	mux.HandleFunc("POST /api/v1/rooms/{id}/extend", h.handleExtendSession)
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// handleCreateGuestRoom — POST /api/v1/rooms
//
// Body: {"title": "My room", "host_guest_id": "<uuid>"}
// Response 201: Room JSON
//
// guest_id is generated client-side or via a /guests endpoint.
// No auth middleware is applied to this route (guest tier is public).
func (h *Handler) handleCreateGuestRoom(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title  string `json:"title"`
		HostID string `json:"host_guest_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if strings.TrimSpace(body.HostID) == "" {
		writeError(w, http.StatusBadRequest, "host_guest_id is required")
		return
	}

	room, err := h.svc.CreateGuestRoom(r.Context(), body.HostID, body.Title)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, room)
}

// handleListRooms — GET /api/v1/rooms?host_guest_id=<uuid>
//
// Lists every room created by the given guest host, newest first, enriched with
// participant count and transcript/summary availability. Powers the meeting history hub.
// Response 200: []RoomListItem
func (h *Handler) handleListRooms(w http.ResponseWriter, r *http.Request) {
	hostGuestID := r.URL.Query().Get("host_guest_id")
	if strings.TrimSpace(hostGuestID) == "" {
		writeError(w, http.StatusBadRequest, "host_guest_id query parameter is required")
		return
	}

	rooms, err := h.svc.ListRoomsByHost(r.Context(), hostGuestID)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, rooms)
}

// handleGetRoom — GET /api/v1/rooms/{id}
// Response 200: Room JSON
func (h *Handler) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	id, ok := h.resolveRoomID(w, r)
	if !ok {
		return
	}

	room, err := h.svc.GetRoom(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, room)
}

// handleEndRoom — DELETE /api/v1/rooms/{id}
//
// Query param: ?guest_id=<uuid>  (the requesting participant's identity)
// Only the host (matching host_guest_id on the room) may end it.
// Response 204: No Content
func (h *Handler) handleEndRoom(w http.ResponseWriter, r *http.Request) {
	id, ok := h.resolveRoomID(w, r)
	if !ok {
		return
	}

	guestID := r.URL.Query().Get("guest_id")
	if guestID == "" {
		writeError(w, http.StatusBadRequest, "guest_id query parameter is required")
		return
	}

	if err := h.svc.EndRoom(r.Context(), id, guestID); err != nil {
		writeServiceError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleIssueGuestToken — GET /api/v1/rooms/{id}/token
//
// Query params:
//
//	guest_id     = participant's unique identity (UUID)
//	display_name = human-readable name shown in participant list
//	is_host      = "true" if this participant created the room
//
// Response 200: {"token": "<livekit-jwt>", "livekit_host": "wss://..."}
//
// This is the security boundary: plan enforcement, session expiry, and participant
// cap are all checked here before any token is issued.
func (h *Handler) handleIssueGuestToken(w http.ResponseWriter, r *http.Request) {
	id, ok := h.resolveRoomID(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	guestID := q.Get("guest_id")
	displayName := q.Get("display_name")
	isHostStr := q.Get("is_host")

	if guestID == "" {
		writeError(w, http.StatusBadRequest, "guest_id is required")
		return
	}
	if displayName == "" {
		displayName = "Guest"
	}
	isHost := strings.EqualFold(isHostStr, "true")

	resp, err := h.svc.IssueGuestToken(r.Context(), id, guestID, displayName, isHost)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleExtendSession — POST /api/v1/rooms/{id}/extend
//
// Body: {"guest_id": "<uuid>"}
// Extends the session by 50% of the original duration (15 min for a 30-min session).
// Single-use per room. Returns 409 if already used.
// Response 200: {"extended": true, "message": "Session extended by 15 minutes"}
func (h *Handler) handleExtendSession(w http.ResponseWriter, r *http.Request) {
	id, ok := h.resolveRoomID(w, r)
	if !ok {
		return
	}

	var body struct {
		GuestID string `json:"guest_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.GuestID == "" {
		writeError(w, http.StatusBadRequest, "guest_id is required")
		return
	}

	if err := h.svc.ExtendGuestSession(r.Context(), id, body.GuestID); err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"extended": true,
		"message":  "Session extended",
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// resolveRoomID extracts the {id} path segment and resolves it to a numeric room id.
//
// The segment may be either the numeric DB id (e.g. "42") or the LiveKit room name
// (e.g. "guest-90c54f16"). The client uses the room name as the shareable identifier,
// so a non-numeric segment is looked up by name rather than rejected with 400.
func (h *Handler) resolveRoomID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")

	if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
		return id, true
	}

	room, err := h.svc.GetRoomByName(r.Context(), raw)
	if err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			writeError(w, http.StatusNotFound, "room not found")
		} else {
			writeError(w, http.StatusBadRequest, "invalid room id")
		}
		return 0, false
	}
	return room.ID, true
}

// writeServiceError maps domain sentinel errors to HTTP status codes.
// errors.Is unwraps the error chain so wrapped sentinels are matched correctly.
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "room not found")
	case errors.Is(err, ErrRoomEnded):
		writeError(w, http.StatusConflict, "room has already ended")
	case errors.Is(err, ErrRoomNotLive):
		writeError(w, http.StatusConflict, "room is not live yet")
	case errors.Is(err, ErrPlanExceeded):
		writeError(w, http.StatusForbidden, "participant limit reached for guest tier")
	case errors.Is(err, ErrSessionExpired):
		writeError(w, http.StatusGone, "session duration limit reached")
	case errors.Is(err, ErrExtendOnce):
		writeError(w, http.StatusConflict, "session extension already used")
	case errors.Is(err, ErrInvalidRole):
		writeError(w, http.StatusBadRequest, "invalid participant role")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

// writeError encodes a JSON error response.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeJSON encodes any value as JSON with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
