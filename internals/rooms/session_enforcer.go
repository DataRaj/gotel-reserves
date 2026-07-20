package rooms

import (
	"context"
	"log"
	"time"

	"recallo/db"
)

type SessionEnforcer struct {
	svc      *Service
	interval time.Duration // how often to scan (default: 1 minute)
}

// NewSessionEnforcer constructs the enforcer.
// interval: how often to check. 1*time.Minute is appropriate for production;
// tests may use shorter intervals.
func NewSessionEnforcer(svc *Service, interval time.Duration) *SessionEnforcer {
	return &SessionEnforcer{
		svc:      svc,
		interval: interval,
	}
}

// Run starts the enforcement loop. It blocks until ctx is cancelled.
// Call as: go enforcer.Run(ctx)
//
// On each tick:
//  1. Query DB for all 'live' guest rooms where (started_at + session_duration_mins) ≤ now.
//  2. For each expired room, call service.EndRoom.
//     EndRoom calls livekit.DeleteRoom → LiveKit fires room_finished webhook → DB updated.
//  3. If EndRoom fails (LiveKit unreachable), the reconciler will catch it next cycle.
func (e *SessionEnforcer) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	log.Printf("[session-enforcer] started — checking every %s", e.interval)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[session-enforcer] stopped")
			return
		case <-ticker.C:
			e.enforceAll(ctx)
		}
	}
}

// enforceAll fetches and processes all expired rooms in one tick.
// Errors per-room are logged and skipped — one bad room doesn't block others.
func (e *SessionEnforcer) enforceAll(ctx context.Context) {
	rooms, err := fetchExpiredGuestRooms(ctx)
	if err != nil {
		log.Printf("[session-enforcer] db query error: %v", err)
		return
	}

	for _, room := range rooms {
		if err := e.svc.EndRoom(ctx, room.ID, room.HostGuestID); err != nil {
			// ErrRoomEnded means it was already ended by a concurrent request — harmless.
			log.Printf("[session-enforcer] end room %d error: %v", room.ID, err)
		} else {
			log.Printf("[session-enforcer] ended expired guest room id=%d name=%s", room.ID, room.LiveKitRoomName)
		}
	}
}

// fetchExpiredGuestRooms queries for all live guest rooms whose effective session
// window (started_at + session_duration_mins minutes, which accounts for extensions)
// has passed. Using INTERVAL arithmetic in Postgres to avoid timezone bugs.
//
// session_duration_mins is stored on the room row and updated when a guest extends.
// This means the query correctly handles extended sessions without extra logic here.
var fetchExpiredGuestRooms = func(ctx context.Context) ([]*Room, error) {
	const query = `
		SELECT id, livekit_room_name, host_guest_id, title, status, tier,
		       started_at, ended_at, created_at, extend_used
		FROM rooms
		WHERE tier = 'guest'
		  AND status = 'live'
		  AND started_at IS NOT NULL
		  AND started_at + (session_duration_mins * INTERVAL '1 minute') <= NOW()
	`
	rows, err := db.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rooms []*Room
	for rows.Next() {
		var r Room
		if err := rows.Scan(
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
		); err != nil {
			return nil, err
		}
		rooms = append(rooms, &r)
	}
	return rooms, rows.Err()
}
