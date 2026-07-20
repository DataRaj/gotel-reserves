package models

import "recallo/db"

type UserSearchResult struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Avatar string `json:"avatar,omitempty"`
}

func SearchMutualUsers(userID int64, query string) ([]*UserSearchResult, error) {
	_, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	// TODO(you): run the query above, scan rows into []*UserSearchResult.
	return []*UserSearchResult{}, nil
}
