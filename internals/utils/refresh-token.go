package utils

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"recallo/db"
	"recallo/internals/models"
)

func GenerateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.Strict().EncodeToString(b), nil
}

func UpdateRefreshToken(userID int64, platform, refreshToken string) error {
	db, err := db.GetDB()
	if err != nil {
		return err
	}

	now := time.Now()

	switch platform {
	case PlatformWeb:
		_, err = db.Exec(
			`UPDATE users SET refresh_token_web = $1, refresh_token_web_updated_at = $2 WHERE id = $3`,
			refreshToken, now, userID,
		)
	case PlatformMobile:
		_, err = db.Exec(
			`UPDATE users SET refresh_token_mobile = $1, refresh_token_mobile_updated_at = $2 WHERE id = $3`,
			refreshToken, now, userID,
		)
	default:
		return errors.New("invalid platform")
	}

	return err
}

// DeleteUserRefreshToken clears the stored refresh token for the given platform.
func DeleteUserRefreshToken(userID int64, platform string) error {
	db, err := db.GetDB()
	if err != nil {
		return err
	}

	switch platform {
	case PlatformWeb:
		_, err = db.Exec(
			`UPDATE users SET refresh_token_web = NULL, refresh_token_web_updated_at = NULL WHERE id = $1`,
			userID,
		)
	case PlatformMobile:
		_, err = db.Exec(
			`UPDATE users SET refresh_token_mobile = NULL, refresh_token_mobile_updated_at = NULL WHERE id = $1`,
			userID,
		)
	default:
		return errors.New("invalid platform")
	}

	return err
}

// GetUserByRefreshToken looks up a user by their stored refresh token for the given platform.
func GetUserByRefreshToken(refreshToken, platform string) (*models.User, error) {
	db, err := db.GetDB()
	if err != nil {
		return nil, err
	}

	var user models.User
	var query string

	switch platform {
	case PlatformWeb:
		query = `
			SELECT id, name, email, password,
			       COALESCE(refresh_token_web, ''),
			       COALESCE(refresh_token_web_updated_at, '1970-01-01'),
			       COALESCE(refresh_token_mobile, ''),
			       COALESCE(refresh_token_mobile_updated_at, '1970-01-01'),
			       created_at, updated_at
			FROM users WHERE refresh_token_web = $1`
	case PlatformMobile:
		query = `
			SELECT id, name, email, password,
			       COALESCE(refresh_token_web, ''),
			       COALESCE(refresh_token_web_updated_at, '1970-01-01'),
			       COALESCE(refresh_token_mobile, ''),
			       COALESCE(refresh_token_mobile_updated_at, '1970-01-01'),
			       created_at, updated_at
			FROM users WHERE refresh_token_mobile = $1`
	default:
		return nil, errors.New("invalid platform")
	}

	err = db.QueryRow(query, refreshToken).Scan(
		&user.ID,
		&user.Name,
		&user.Email,
		&user.Password,
		&user.RefreshTokenForWeb,
		&user.RefreshTokenForWebUpdatedAt,
		&user.RefreshTokenForApp,
		&user.RefreshTokenForAppUpdatedAt,
		&user.CreatedAt,
		&user.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	return &user, nil
}
