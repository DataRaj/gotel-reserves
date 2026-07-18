package models

import (
	"time"

	"recallo/db"
)

type Message struct {
	ID        int64 `json:"id"`
	FromID    int64 `json:"from_id"`
	PrivateID int64 `json:"private_id"`

	MessageType string `json:"message_type"`
	Content     string `json:"content"`

	// GifURL is not a DB column — for gif messages the URL lives in Content.
	// This field is populated in code so clients receive a gif_url both on the
	// live WS broadcast and on REST history reads.
	GifURL string `json:"gif_url,omitempty"`

	Delivered bool `json:"delivered"`
	Read      bool `json:"read"`

	CreatedAt time.Time `json:"created_at"`
}

func boolToInt(val bool) int {
	if val {
		return 1
	}
	return 0
}

func CreateMessage(m *Message) error {
	db, err := db.GetDB()
	if err != nil {
		return err
	}

	// RETURNING id populates m.ID before the caller broadcasts the message,
	// so live WS payloads carry a real primary key instead of the zero value.
	err = db.QueryRow(
		"INSERT INTO messages (from_id, private_id, message_type, content, delivered, read) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id",
		m.FromID,
		m.PrivateID,
		m.MessageType,
		m.Content,
		boolToInt(m.Delivered),
		boolToInt(m.Read),
	).Scan(&m.ID)
	if err != nil {
		return err
	}

	return nil
}

// hydrateGifURL mirrors the stored content into GifURL for gif messages so
// clients get a gif_url on both REST and WS payloads.
func (m *Message) hydrateGifURL() {
	if m.MessageType == "gif" {
		m.GifURL = m.Content
	}
}

func GetMessagesByPrivateID(privateId int64, page, limit int) ([]*Message, error) {
	db, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	offset := (page - 1) * limit

	rows, err := db.Query(
		`
		SELECT id, from_id, private_id, message_type, content, delivered, read, created_at
		FROM messages
		WHERE private_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
		`,
		privateId,
		limit,
		offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []*Message
	for rows.Next() {
		var m Message
		var deliveredInt, readInt int
		err := rows.Scan(
			&m.ID,
			&m.FromID,
			&m.PrivateID,
			&m.MessageType,
			&m.Content,
			&deliveredInt,
			&readInt,
			&m.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		m.Delivered = deliveredInt == 1
		m.Read = readInt == 1
		m.hydrateGifURL()
		messages = append(messages, &m)
	}

	return messages, nil
}

func GetMessageByID(messageId int64) (*Message, error) {
	db, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	var m Message
	var deliveredInt, readInt int
	err = db.QueryRow(
		"SELECT id, from_id, private_id, message_type, content, delivered, read, created_at FROM messages WHERE id = $1",
		messageId,
	).Scan(
		&m.ID,
		&m.FromID,
		&m.PrivateID,
		&m.MessageType,
		&m.Content,
		&deliveredInt,
		&readInt,
		&m.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	m.Delivered = deliveredInt == 1
	m.Read = readInt == 1
	m.hydrateGifURL()

	return &m, nil
}

func GetUndeliveredMessagesByPrivateID(privateId int64) ([]*Message, error) {
	db, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(
		`
		SELECT id, from_id, private_id, message_type, content, delivered, read, created_at
		FROM messages
		WHERE private_id = $1 AND delivered = 0
		ORDER BY created_at DESC
		`,
		privateId,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	messages := []*Message{}
	for rows.Next() {
		var m Message
		var deliveredInt, readInt int
		err := rows.Scan(
			&m.ID,
			&m.FromID,
			&m.PrivateID,
			&m.MessageType,
			&m.Content,
			&deliveredInt,
			&readInt,
			&m.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		m.Delivered = deliveredInt == 1
		m.Read = readInt == 1
		m.hydrateGifURL()
		messages = append(messages, &m)
	}

	return messages, nil
}

// MarkAllIncomingMessagesAsDelivered silently marks every undelivered message
// sent TO userID (not by them) as delivered=1 in a single atomic query.
// Called on WebSocket connect so no goroutine-level looping is needed.
func MarkAllIncomingMessagesAsDelivered(userID int64) error {
	db, err := db.GetDB()
	if err != nil {
		return err
	}

	_, err = db.Exec(`
		UPDATE messages
		SET delivered = 1
		WHERE delivered = 0
		  AND from_id != $1
		  AND private_id IN (
		      SELECT id FROM privates WHERE user1_id = $2 OR user2_id = $3
		  )
	`, userID, userID, userID)

	return err
}

func MarkMessageAsDelivered(messageId int64) error {
	db, err := db.GetDB()
	if err != nil {
		return err
	}

	_, err = db.Exec("UPDATE messages SET delivered = 1 WHERE id = $1", messageId)
	if err != nil {
		return err
	}

	return nil
}

func MarkMessageAsRead(messageId int64) error {
	db, err := db.GetDB()
	if err != nil {
		return err
	}

	_, err = db.Exec("UPDATE messages SET read = 1 WHERE id = $1", messageId)
	if err != nil {
		return err
	}

	return nil
}
