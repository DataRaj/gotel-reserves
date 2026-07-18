package models

import (
	"database/sql"
	"errors"
	"time"

	"recallo/db"
	"recallo/internals/logger"
)

type Private struct {
	ID        int64     `json:"id"`
	User1     string    `json:"user1"`
	User2     string    `json:"user2"`
	User1Name string    `json:"user1_name"`
	User2Name string    `json:"user2_name"`
	CreatedAt time.Time `json:"created_at"`
}

func GetPrivateByID(privateId int64) (*Private, error) {
	db, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	var private Private
	err = db.QueryRow(
		`
		SELECT p.id, p.user1_id, p.user2_id, u1.name, u2.name, p.created_at
		FROM privates p
		JOIN users u1 ON p.user1_id = u1.id
		JOIN users u2 ON p.user2_id = u2.id
		WHERE p.id = $1
		`,
		privateId,
	).Scan(
		&private.ID,
		&private.User1,
		&private.User2,
		&private.User1Name,
		&private.User2Name,
		&private.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	return &private, nil
}

func GetPrivateByUsers(user1Id, user2Id int64) (*Private, error) {
	if user1Id > user2Id {
		user1Id, user2Id = user2Id, user1Id
	}

	db, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	var private Private
	err = db.QueryRow("SELECT id, user1_id, user2_id, created_at FROM privates WHERE user1_id = $1 AND user2_id = $2", user1Id, user2Id).Scan(
		&private.ID,
		&private.User1,
		&private.User2,
		&private.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	return &private, nil
}

func GetPrivatesForUser(userId int64) ([]*Private, error) {
	db, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(
		`
		SELECT p.id, p.user1_id, p.user2_id, u1.name, u2.name, p.created_at
		FROM privates p
		JOIN users u1 ON p.user1_id = u1.id
		JOIN users u2 ON p.user2_id = u2.id
		WHERE p.user1_id = $1 OR p.user2_id = $2
		ORDER BY p.created_at DESC
		`,
		userId,
		userId,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var privates []*Private
	for rows.Next() {
		var private Private
		err := rows.Scan(
			&private.ID,
			&private.User1,
			&private.User2,
			&private.User1Name,
			&private.User2Name,
			&private.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		privates = append(privates, &private)
	}

	logger.Info(" here are the priavates data from the conversation api", &privates)
	return privates, nil
}

func CreatePrivate(user1Id, user2Id int64) (*Private, error) {
	if user1Id == user2Id {
		return nil, errors.New("cannot create private chat with the same user")
	}

	if user1Id > user2Id {
		user1Id, user2Id = user2Id, user1Id
	}

	db, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	existingPrivate, err := GetPrivateByUsers(user1Id, user2Id)
	if err != nil {
		return nil, err
	}
	if existingPrivate != nil {
		return existingPrivate, nil
	}

	var privateId int64
	err = db.QueryRow("INSERT INTO privates (user1_id, user2_id) VALUES ($1, $2) RETURNING id", user1Id, user2Id).Scan(&privateId)
	if err != nil {
		return nil, err
	}

	return GetPrivateByID(privateId)
}
