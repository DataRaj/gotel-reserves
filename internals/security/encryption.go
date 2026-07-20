package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

func Encrypt(plaintext []byte, secretKey []byte) ([]byte, error) {
	if len(secretKey) != 32 {
		return nil, errors.New("secret key must be exactly 32 bytes")
	}

	block, err := aes.NewCipher(secretKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	// Seal appends the encrypted data and auth tag to the nonce
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func Decrypt(ciphertext []byte, secretKey []byte) ([]byte, error) {
	if len(secretKey) != 32 {
		return nil, errors.New("secret key must be exactly 32 bytes")
	}

	block, err := aes.NewCipher(secretKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}

	nonce := ciphertext[:gcm.NonceSize()]
	actualCiphertext := ciphertext[gcm.NonceSize():]

	return gcm.Open(nil, nonce, actualCiphertext, nil)
}
