// Package crypto provides the authenticated symmetric encryption used to
// protect gateway credentials at rest: AES-256-GCM with the ciphertext bound to
// caller-supplied additional authenticated data (AAD).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// ErrKeyLength is returned when the encryption key is not 32 bytes (AES-256).
var ErrKeyLength = errors.New("crypto: encryption key must be 32 bytes")

// Encrypt seals plaintext with AES-256-GCM under key, binding aad. The output is
// base64(nonce || ciphertext || tag).
func Encrypt(key, aad, plaintext []byte) (string, error) {
	if len(key) != 32 {
		return "", ErrKeyLength
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("crypto: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, plaintext, aad)
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt, returning an error on tampering or an aad mismatch.
func Decrypt(key, aad []byte, ciphertextB64 string) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrKeyLength
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode ciphertext: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("crypto: ciphertext too short")
	}
	nonce, sealed := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt (aad mismatch or tampering): %w", err)
	}
	return plaintext, nil
}
