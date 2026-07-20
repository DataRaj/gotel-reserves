package rooms

import "errors"

var (
	// ErrRoomNotFound is returned when a room ID is not present in the DB.
	ErrRoomNotFound = errors.New("room not found")

	// ErrRoomEnded is returned when an operation is attempted on an ended room.
	// Callers should map this to 409 Conflict (the room exists but is closed).
	ErrRoomEnded = errors.New("room has already ended")

	// ErrRoomNotLive is returned when a room is still in 'draft' status and
	// an operation requires it to be 'live' (e.g. generating a viewer token
	// before the host has started the session).
	ErrRoomNotLive = errors.New("room is not live yet")

	// ErrPlanExceeded is returned when the participant count in a room has
	// reached the tier's MaxParticipants cap. Maps to 403 Forbidden.
	ErrPlanExceeded = errors.New("participant limit for this plan has been reached")

	// ErrSessionExpired is returned when the wall-clock time elapsed since
	// room_started exceeds the guest tier session duration (default 30 min).
	// The session enforcer fires this before calling DeleteRoom.
	ErrSessionExpired = errors.New("session duration limit has been reached")

	// ErrExtendOnce is returned when a guest room has already been extended
	// once. The extend endpoint is single-use per room per guest tier.
	ErrExtendOnce = errors.New("session extension has already been used for this room")

	// ErrInvalidRole is returned when an unrecognised participant role string
	// is passed to token generation.
	ErrInvalidRole = errors.New("invalid participant role")
)
