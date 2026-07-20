package models

import (
	"encoding/json"
	"os"

	"recallo/db"
	"recallo/internals/security"

	"golang.org/x/oauth2"
)

func getEncryptionKey() []byte {
	key := os.Getenv("OAUTH_ENCRYPTION_KEY")
	if len(key) >= 32 {
		return []byte(key[:32])
	}
	// Fallback 32-byte key (WARNING: only for local dev)
	return []byte("12345678901234567890123456789012")
}

func SaveOAuthToken(userID int64, provider, providerUserID string, token *oauth2.Token) error {
	data, err := json.Marshal(token)
	if err != nil {
		return err
	}

	encryptedToken, err := security.Encrypt(data, getEncryptionKey())
	if err != nil {
		return err
	}

	query := `
		INSERT INTO oauth_connections (user_id, provider, provider_user_id, encrypted_token, updated_at)
		VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP)
		ON CONFLICT (provider, provider_user_id) 
		DO UPDATE SET 
			encrypted_token = EXCLUDED.encrypted_token,
			updated_at = CURRENT_TIMESTAMP
	`
	_, err = db.DB.Exec(query, userID, provider, providerUserID, encryptedToken)
	return err
}

// GetOAuthToken loads and decrypts an OAuth2 token from the database for a user
func GetOAuthToken(userID int64, provider string) (*oauth2.Token, error) {
	var encryptedToken []byte
	query := `SELECT encrypted_token FROM oauth_connections WHERE user_id = $1 AND provider = $2`
	
	err := db.DB.QueryRow(query, userID, provider).Scan(&encryptedToken)
	if err != nil {
		return nil, err
	}

	decryptedData, err := security.Decrypt(encryptedToken, getEncryptionKey())
	if err != nil {
		return nil, err
	}

	var token oauth2.Token
	if err := json.Unmarshal(decryptedData, &token); err != nil {
		return nil, err
	}

	return &token, nil
}
