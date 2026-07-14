package rooms

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"fmt"
	"time"

	"recallo/db"
	"recallo/internals/configs"
	lktypes "recallo/internals/livekit"
)

// Service owns all business logic for the rooms domain.
//
// Dependencies are passed explicitly at construction time — no package-level globals.
// This makes the service fully testable in isolation: swap LiveKitService with a fake,
// swap the DB with a test DB, and every method is exercisable without a real LiveKit Cloud.
//
// Design: the service layer never writes raw SQL — it delegates all DB access to the
// repository functions in this package (repository.go). Business rules live here;
// persistence mechanics live there.
type Service struct {
	lk       LiveKitService // interface — not the concrete *livekit.Service type
	guestCfg configs.GuestTierConfig
}

// NewService constructs the rooms Service.
// lk must satisfy the LiveKitService interface declared in this package.
func NewService(lk LiveKitService, guestCfg configs.GuestTierConfig) *Service {
	return &Service{
		lk:       lk,
		guestCfg: guestCfg,
	}
}

// CreateGuestRoom creates a room for the non-login (guest) tier.
//
// Sequence:
//  1. Generate a unique LiveKit room name (UUID-derived slug).
//  2. Pre-create the LiveKit room with max_participants and empty_timeout baked in.
//     This means LiveKit itself enforces the participant cap — not just our token check.
//  3. Insert a DB record with status='draft' and tier='guest'.
//
// The room transitions to 'live' when the room_started webhook arrives (not here).
// The room is auto-ended by the session enforcer after GuestTier.SessionDurationMins.
//
// HostID: for guest tier rooms the host is identified by a generated guest UUID
// (created client-side or by a /guests/register-anonymous endpoint), not a DB user ID.
// For now we accept the hostGuestID as a string identity.
func (s *Service) CreateGuestRoom(ctx context.Context, hostGuestID, title string) (*Room, error) {
	// Generate a collision-resistant room name for LiveKit.
	// Pattern: "guest-<10-char hex>" — readable in LiveKit dashboard.
	roomName, err := generateRoomName("guest")
	if err != nil {
		return nil, fmt.Errorf("rooms.Service.CreateGuestRoom: generate room name: %w", err)
	}

	// Pre-create in LiveKit with plan limits as infrastructure constraints.
	// empty_timeout=300: room survives 5 min empty before LiveKit fires room_finished.
	createParams := lktypes.CreateRoomParams{
		RoomName:         roomName,
		MaxParticipants:  uint32(s.guestCfg.MaxParticipants),
		EmptyTimeoutSecs: 300,
		Metadata:         fmt.Sprintf(`{"tier":"guest","host":"%s"}`, hostGuestID),
	}
	if err := s.lk.CreateRoom(ctx, createParams); err != nil {
		return nil, fmt.Errorf("rooms.Service.CreateGuestRoom: livekit create: %w", err)
	}

	// Persist room record with status='draft'.
	// We do NOT set status='live' here — that comes from the room_started webhook.
	room, err := insertRoom(ctx, insertRoomParams{
		LiveKitRoomName: roomName,
		HostGuestID:     hostGuestID,
		Title:           title,
		Status:          RoomStatusDraft,
		Tier:            TierGuest,
	})
	if err != nil {
		// Best-effort cleanup: try to delete the LiveKit room we just created.
		// If this also fails, the reconciler will detect the orphaned room.
		_ = s.lk.DeleteRoom(context.Background(), roomName)
		return nil, fmt.Errorf("rooms.Service.CreateGuestRoom: db insert: %w", err)
	}

	return room, nil
}

// GetRoom fetches a room by ID. Returns ErrRoomNotFound if not present.
func (s *Service) GetRoom(ctx context.Context, roomID int64) (*Room, error) {
	room, err := getRoomByID(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("rooms.Service.GetRoom: %w", err)
	}
	return room, nil
}

// GetRoomByName fetches a room by its LiveKit room name (e.g. "guest-90c54f16").
// Returns ErrRoomNotFound if not present.
//
// The client uses livekit_room_name — not the numeric DB id — as the shareable
// room identifier in URLs, so lookups by name are a first-class access path.
func (s *Service) GetRoomByName(ctx context.Context, roomName string) (*Room, error) {
	room, err := getRoomByLiveKitName(ctx, roomName)
	if err != nil {
		return nil, fmt.Errorf("rooms.Service.GetRoomByName: %w", err)
	}
	return room, nil
}

// ListRoomsByHost returns every room created by the given guest host, newest first.
// Used by the meeting history hub. Each item is enriched with participant count and
// transcript/summary availability so the UI can render status badges without N+1 calls.
func (s *Service) ListRoomsByHost(ctx context.Context, hostGuestID string) ([]*RoomListItem, error) {
	rooms, err := listRoomsByHost(ctx, hostGuestID)
	if err != nil {
		return nil, fmt.Errorf("rooms.Service.ListRoomsByHost: %w", err)
	}
	return rooms, nil
}

// IssueGuestToken validates plan constraints and returns a LiveKit JWT for a guest participant.
//
// Pre-token checks (the token is the security boundary — all enforcement happens here):
//  1. Room must exist and not be ended.
//  2. Current live participant count must be < GuestTier.MaxParticipants.
//  3. Session must not have expired (checked against started_at + duration).
//
// Token is valid for the room's remaining session time (not a fixed 6h window)
// so tokens don't outlive guest sessions.
//
// guestIdentity: the guest UUID. Must be unique per participant per room.
// displayName: shown to other participants via SDK metadata read.
// isHost: true only for the participant who called CreateGuestRoom (same guestIdentity).
func (s *Service) IssueGuestToken(
	ctx context.Context,
	roomID int64,
	guestIdentity string,
	displayName string,
	isHost bool,
) (*TokenResponse, error) {
	room, err := getRoomByID(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("rooms.Service.IssueGuestToken: %w", err)
	}

	// ── Guard: room must be joinable ──────────────────────────────────────────
	if room.Status == RoomStatusEnded {
		return nil, fmt.Errorf("rooms.Service.IssueGuestToken: %w", ErrRoomEnded)
	}

	// ── Guard: participant cap ────────────────────────────────────────────────
	// We query LiveKit for the real-time count (DB attendance log lags behind).
	// Returns 0 on error (e.g. room not yet in LiveKit — draft state) so we don't
	// block tokens for a room that hasn't had its first participant yet.
	count, err := s.lk.ListParticipantCount(ctx, room.LiveKitRoomName)
	if err != nil {
		return nil, fmt.Errorf("rooms.Service.IssueGuestToken: count participants: %w", err)
	}
	if count >= s.guestCfg.MaxParticipants {
		return nil, fmt.Errorf("rooms.Service.IssueGuestToken: %w", ErrPlanExceeded)
	}

	// ── Guard: session expiry (only once room is live) ────────────────────────
	if room.Status == RoomStatusLive && room.StartedAt != nil {
		sessionDeadline := room.StartedAt.Add(
			time.Duration(s.guestCfg.SessionDurationMins) * time.Minute,
		)
		if time.Now().After(sessionDeadline) {
			return nil, fmt.Errorf("rooms.Service.IssueGuestToken: %w", ErrSessionExpired)
		}
	}

	// ── Token TTL: remaining session time (not a fixed 6h window) ─────────────
	// Guests should not hold tokens that outlive their session cap.
	var ttl time.Duration
	if room.Status == RoomStatusLive && room.StartedAt != nil {
		remaining := time.Until(room.StartedAt.Add(
			time.Duration(s.guestCfg.SessionDurationMins) * time.Minute,
		))
		if remaining <= 0 {
			return nil, fmt.Errorf("rooms.Service.IssueGuestToken: %w", ErrSessionExpired)
		}
		// Add 60s buffer so the token doesn't expire right as the session ends.
		ttl = remaining + 60*time.Second
	} else {
		// Room is still in 'draft' (host hasn't connected yet).
		// Issue a token valid for the full guest session window.
		ttl = time.Duration(s.guestCfg.SessionDurationMins)*time.Minute + 60*time.Second
	}

	// ── Determine role ────────────────────────────────────────────────────────
	role := lktypes.RoleSpeaker // default for guest meeting
	if isHost {
		role = lktypes.RoleHost
	}

	// ── Map quality setting ───────────────────────────────────────────────────
	quality := mapVideoQuality(s.guestCfg.MaxVideoQuality)

	// ── Mint token ────────────────────────────────────────────────────────────
	tokenParams := lktypes.GenerateTokenParams{
		RoomName: room.LiveKitRoomName,
		Identity: guestIdentity,
		Role:     role,
		Metadata: lktypes.ParticipantMetadata{
			DisplayName: displayName,
			Plan:        "guest",
		},
		TTL:             ttl,
		MaxVideoQuality: quality,
	}

	token, err := s.lk.GenerateToken(tokenParams)
	if err != nil {
		return nil, fmt.Errorf("rooms.Service.IssueGuestToken: %w", err)
	}

	return &TokenResponse{
		Token:       token,
		LiveKitHost: s.lk.Host(),
	}, nil
}

// EndRoom tears down a guest room early (e.g. host presses "End call").
// Delegates the actual DB status update to the room_finished webhook handler —
// this only initiates the teardown with LiveKit.
func (s *Service) EndRoom(ctx context.Context, roomID int64, requestingGuestID string) error {
	room, err := getRoomByID(ctx, roomID)
	if err != nil {
		return fmt.Errorf("rooms.Service.EndRoom: %w", err)
	}
	if room.Status == RoomStatusEnded {
		return fmt.Errorf("rooms.Service.EndRoom: %w", ErrRoomEnded)
	}

	// Only the host may end the room.
	if room.HostGuestID != requestingGuestID {
		return fmt.Errorf("rooms.Service.EndRoom: only the host may end the room")
	}

	if err := s.lk.DeleteRoom(ctx, room.LiveKitRoomName); err != nil {
		return fmt.Errorf("rooms.Service.EndRoom: livekit delete: %w", err)
	}

	// Do NOT set status='ended' here. Wait for room_finished webhook.
	// If the webhook never arrives, the reconciler will fix it.
	return nil
}

// ExtendGuestSession adds a one-time extension to a guest room's session duration.
// The extension amount is fixed by policy (not caller-controlled) and may only be
// used once per room. Additional calls return ErrExtendOnce.
//
// Extension amount: half of the original session duration (15 min for a 30-min session).
// This is a product decision baked into the service, not the config, because it's
// a business rule not an operational tuning knob.
func (s *Service) ExtendGuestSession(ctx context.Context, roomID int64, requestingGuestID string) error {
	room, err := getRoomByID(ctx, roomID)
	if err != nil {
		return fmt.Errorf("rooms.Service.ExtendGuestSession: %w", err)
	}
	if room.Status == RoomStatusEnded {
		return fmt.Errorf("rooms.Service.ExtendGuestSession: %w", ErrRoomEnded)
	}
	if room.ExtendUsed {
		return fmt.Errorf("rooms.Service.ExtendGuestSession: %w", ErrExtendOnce)
	}

	// Extension = half the original session duration. For 30-min sessions: +15 min.
	extensionMins := s.guestCfg.SessionDurationMins / 2
	if err := markExtensionUsed(ctx, roomID, extensionMins); err != nil {
		return fmt.Errorf("rooms.Service.ExtendGuestSession: db: %w", err)
	}

	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// mapVideoQuality converts the human-readable config string to the livekit
// VideoQuality constant. Defaults to medium if the string is unrecognised
// (config validation in LoadConfig catches bad values before startup).
func mapVideoQuality(q string) lktypes.VideoQuality {
	switch q {
	case "low":
		return lktypes.VideoQualityLow
	case "high":
		return lktypes.VideoQualityHigh
	default:
		return lktypes.VideoQualityMedium
	}
}

// generateRoomName produces a collision-resistant room name with the given prefix.
// Pattern: "<prefix>-<8 random hex chars>"
// Example: "guest-3a7f2b1c"
func generateRoomName(prefix string) (string, error) {
	b := make([]byte, 4)
	if _, err := cryptoRandRead(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%x", prefix, b), nil
}

// cryptoRandRead is a thin wrapper around crypto/rand.Read so it can be replaced
// in tests without importing crypto/rand directly in tests.
var cryptoRandRead = func(b []byte) (int, error) {
	return cryptorand.Read(b)
}

// ── Repository call shims (thin wrappers that will call repository.go functions) ──

// insertRoomParams carries all fields needed for a new room DB row.
type insertRoomParams struct {
	LiveKitRoomName string
	HostGuestID     string
	Title           string
	Status          RoomStatus
	Tier            RoomTier
}

// insertRoom persists a new room record. Implemented in repository.go.
// Declared here as a package-level var so tests can swap it for a fake.
var insertRoom = func(ctx context.Context, p insertRoomParams) (*Room, error) {
	query := `
		INSERT INTO rooms (livekit_room_name, host_guest_id, title, status, tier, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		RETURNING id, livekit_room_name, host_guest_id, title, status, tier, started_at, ended_at, created_at, extend_used
	`
	var r Room
	err := db.DB.QueryRowContext(ctx, query,
		p.LiveKitRoomName,
		p.HostGuestID,
		p.Title,
		string(p.Status),
		string(p.Tier),
	).Scan(
		&r.ID,
		&r.LiveKitRoomName,
		&r.HostGuestID,
		&r.Title,
		&r.Status,
		&r.Tier,
		&r.StartedAt,
		&r.EndedAt,
		&r.CreatedAt,
		&r.ExtendUsed,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// getRoomByID fetches a room by primary key. Returns ErrRoomNotFound when absent.
var getRoomByID = func(ctx context.Context, id int64) (*Room, error) {
	query := `
		SELECT id, livekit_room_name, host_guest_id, title, status, tier,
		       started_at, ended_at, created_at, extend_used
		FROM rooms
		WHERE id = $1
	`
	var r Room
	err := db.DB.QueryRowContext(ctx, query, id).Scan(
		&r.ID,
		&r.LiveKitRoomName,
		&r.HostGuestID,
		&r.Title,
		&r.Status,
		&r.Tier,
		&r.StartedAt,
		&r.EndedAt,
		&r.CreatedAt,
		&r.ExtendUsed,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("rooms: %w", ErrRoomNotFound)
		}
		return nil, err
	}
	return &r, nil
}

// getRoomByLiveKitName fetches a room by its unique livekit_room_name.
// Returns ErrRoomNotFound when absent.
var getRoomByLiveKitName = func(ctx context.Context, name string) (*Room, error) {
	query := `
		SELECT id, livekit_room_name, host_guest_id, title, status, tier,
		       started_at, ended_at, created_at, extend_used
		FROM rooms
		WHERE livekit_room_name = $1
	`
	var r Room
	err := db.DB.QueryRowContext(ctx, query, name).Scan(
		&r.ID,
		&r.LiveKitRoomName,
		&r.HostGuestID,
		&r.Title,
		&r.Status,
		&r.Tier,
		&r.StartedAt,
		&r.EndedAt,
		&r.CreatedAt,
		&r.ExtendUsed,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("rooms: %w", ErrRoomNotFound)
		}
		return nil, err
	}
	return &r, nil
}

// listRoomsByHost returns all rooms for a host guest, newest first, enriched with
// participant count and transcript/summary availability for the history hub.
var listRoomsByHost = func(ctx context.Context, hostGuestID string) ([]*RoomListItem, error) {
	query := `
		SELECT r.id, r.livekit_room_name, r.host_guest_id, r.title, r.status, r.tier,
		       r.started_at, r.ended_at, r.created_at, r.extend_used,
		       COALESCE(r.session_duration_mins, 0),
		       (SELECT COUNT(*) FROM room_participants rp WHERE rp.room_livekit_name = r.livekit_room_name),
		       EXISTS(SELECT 1 FROM transcripts t WHERE t.room_livekit_name = r.livekit_room_name),
		       EXISTS(SELECT 1 FROM summaries s WHERE s.room_livekit_name = r.livekit_room_name)
		FROM rooms r
		WHERE r.host_guest_id = $1
		ORDER BY r.created_at DESC
	`
	rows, err := db.DB.QueryContext(ctx, query, hostGuestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]*RoomListItem, 0)
	for rows.Next() {
		var it RoomListItem
		if err := rows.Scan(
			&it.ID,
			&it.LiveKitRoomName,
			&it.HostGuestID,
			&it.Title,
			&it.Status,
			&it.Tier,
			&it.StartedAt,
			&it.EndedAt,
			&it.CreatedAt,
			&it.ExtendUsed,
			&it.SessionDurationMins,
			&it.ParticipantCount,
			&it.HasTranscript,
			&it.HasSummary,
		); err != nil {
			return nil, err
		}
		items = append(items, &it)
	}
	return items, rows.Err()
}

// markExtensionUsed sets extend_used=true and bumps the effective session duration.
var markExtensionUsed = func(ctx context.Context, roomID int64, extraMins int) error {
	_, err := db.DB.ExecContext(ctx, `
		UPDATE rooms
		SET extend_used = TRUE,
		    session_duration_mins = session_duration_mins + $2
		WHERE id = $1
	`, roomID, extraMins)
	return err
}
