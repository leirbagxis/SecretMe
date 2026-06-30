package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

// deriveKey derives a 32-byte AES key from an identity string and salt using HKDF.
// identity is either "user:<userID>" (e.g. "user:123456789") or "username:<name>" (e.g. "username:joao").
func deriveKey(identity string, salt string) []byte {
	ikm := sha256.Sum256([]byte(identity))
	key, err := hkdf.Key(sha256.New, ikm[:], []byte(salt), "secretme-aes-gcm", 32)
	if err != nil {
		// hkdf.Key only errors if keyLength is invalid; 32 is always valid
		panic(fmt.Sprintf("hkdf.Key: %v", err))
	}
	return key
}

// IdentityNumeric returns the identity string for a numeric user ID.
func IdentityNumeric(userID int64) string {
	return fmt.Sprintf("user:%d", userID)
}

// IdentityUsername returns the identity string for a @username (without @).
func IdentityUsername(username string) string {
	return fmt.Sprintf("username:%s", username)
}

// Encrypt encrypts plaintext using AES-GCM with a key derived from the identity string and salt.
func Encrypt(plaintext []byte, identity, salt string) (ciphertext []byte, nonce []byte, err error) {
	key := deriveKey(identity, salt)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("create cipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create GCM: %w", err)
	}

	nonce = make([]byte, aesGCM.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext = aesGCM.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// Decrypt decrypts ciphertext using AES-GCM with a key derived from the identity string and salt.
func Decrypt(ciphertext []byte, nonce []byte, identity, salt string) ([]byte, error) {
	key := deriveKey(identity, salt)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}
