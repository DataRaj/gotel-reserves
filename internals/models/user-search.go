package models

import "recallo/db"

// UserSearchResult is the shape the chat UI expects for search results.
// Keep the json tags aligned with the client's UserSearchResult type
// (id, name, email, avatar).
type UserSearchResult struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Avatar string `json:"avatar,omitempty"`
}

// SearchMutualUsers returns users who share a mutual connection with userID and
// match the free-text query (name/email).
//
// TODO(you): implement the business logic. A "mutual" user is one who either:
//   - shares a row in `privates` (already in a private chat with userID), OR
//   - shared a meeting via `room_participants` (same livekit room).
//
// Suggested query skeleton (adjust to taste):
//
//	SELECT DISTINCT u.id, u.name, u.email, COALESCE(u.avatar_url, '')
//	FROM users u
//	WHERE u.id != $1
//	  AND (u.name ILIKE '%' || $2 || '%' OR u.email ILIKE '%' || $2 || '%')
//	  AND u.id IN ( <mutual-connection subquery over privates / room_participants> )
//	LIMIT 20;
func SearchMutualUsers(userID int64, query string) ([]*UserSearchResult, error) {
	_, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	// TODO(you): run the query above, scan rows into []*UserSearchResult.
	return []*UserSearchResult{}, nil
}
