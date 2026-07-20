package rooms

import (
	"context"
	"time"

	lktypes "recallo/internals/livekit"
)

type LiveKitService interface {
	CreateRoom(ctx context.Context, p lktypes.CreateRoomParams) error

	DeleteRoom(ctx context.Context, roomName string) error

	GenerateToken(p lktypes.GenerateTokenParams) (string, error)

	ListParticipantCount(ctx context.Context, roomName string) (int, error)

	RemoveParticipant(ctx context.Context, roomName, identity string) error

	// Host returns the LiveKit Cloud WSS host URL for returning to clients.
	Host() string
}

// Room is the in-memory representation of a room record.
// Maps 1:1 with the rooms DB table row.
type Room struct {
	ID              int64      `json:"id"`
	HostGuestID     string     `json:"host_guest_id"`     // guest UUID of the room creator
	LiveKitRoomName string     `json:"livekit_room_name"` // stable LiveKit identifier
	Title           string     `json:"title"`
	Status          RoomStatus `json:"status"`
	Tier            RoomTier   `json:"tier"`
	ExtendUsed      bool       `json:"extend_used"` // true after guest has used their one extension
	StartedAt       *time.Time `json:"started_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// RoomListItem is a Room enriched with pipeline/attendance metadata for the
// meeting history hub. It is returned only by the list endpoint, so the base
// Room struct (used everywhere else) stays lean.
type RoomListItem struct {
	ID                  int64      `json:"id"`
	HostGuestID         string     `json:"host_guest_id"`
	LiveKitRoomName     string     `json:"livekit_room_name"`
	Title               string     `json:"title"`
	Status              RoomStatus `json:"status"`
	Tier                RoomTier   `json:"tier"`
	ExtendUsed          bool       `json:"extend_used"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	EndedAt             *time.Time `json:"ended_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	SessionDurationMins int        `json:"session_duration_mins"`
	ParticipantCount    int        `json:"participant_count"`
	HasTranscript       bool       `json:"has_transcript"`
	HasSummary          bool       `json:"has_summary"`
}

type RoomStatus string

const (
	RoomStatusDraft RoomStatus = "draft" // created in DB, LiveKit room pre-created
	RoomStatusLive  RoomStatus = "live"  // room_started webhook received
	RoomStatusEnded RoomStatus = "ended" // room_finished webhook received
)

type RoomTier string

const (
	TierGuest RoomTier = "guest"

	TierStandard = "standard"

	TierPro RoomTier = "pro"
)

// TokenResponse is what the GET /rooms/:id/token endpoint returns to the frontend.
// The frontend uses LiveKitHost + Token to call room.connect(host, token).
type TokenResponse struct {
	Token       string `json:"token"`        // signed LiveKit JWT
	LiveKitHost string `json:"livekit_host"` // wss:// URL for SDK connect call
}
