// Package credentialcipher encrypts stored credentials (community PDS
// passwords and tokens, aggregator OAuth sessions and DPoP keys) with a key
// held by the process rather than by the database, so a database read or a
// backup does not yield the plaintext.
package credentialcipher

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeySize is the number of raw key bytes AES-256 requires.
const KeySize = 32

var (
	// ErrInvalidKey reports a key that is not exactly KeySize bytes.
	ErrInvalidKey = errors.New("credentialcipher: key must be 32 bytes")
	// ErrInvalidCiphertext reports a value that is too short, tampered with,
	// or sealed under a different key or context.
	ErrInvalidCiphertext = errors.New("credentialcipher: invalid ciphertext")
	// ErrUnsupportedVersion reports invalid ciphertext whose leading version
	// byte this build does not understand.
	ErrUnsupportedVersion = fmt.Errorf("%w: unsupported ciphertext version", ErrInvalidCiphertext)
)

// Version is the leading byte of every value this package writes. Migration
// 046 and the startup conversion pass compare against it; pgcrypto output
// starts with 0xC3, so the two never collide.
const Version = byte(0x01)

// Cipher seals and opens credential values with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from exactly KeySize raw key bytes.
func New(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKey, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: initialize AES", ErrInvalidKey)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credentialcipher: initialize GCM: %w", err)
	}

	return &Cipher{aead: aead}, nil
}

// NewFromBase64 builds a Cipher from a standard-base64 encoding of KeySize bytes.
func NewFromBase64(encoded string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid base64", ErrInvalidKey)
	}
	return New(key)
}

// Encrypt seals plaintext bound to context. The same context must be passed
// to Decrypt; a value moved to a different row or column will not open.
func (c *Cipher) Encrypt(plaintext, context string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("credentialcipher: generate nonce: %w", err)
	}

	sealed := make([]byte, 1+len(nonce))
	sealed[0] = Version
	copy(sealed[1:], nonce)
	authenticatedData := append([]byte{Version}, context...)
	return c.aead.Seal(sealed, nonce, []byte(plaintext), authenticatedData), nil
}

// Decrypt opens a value produced by Encrypt under the same key and context.
func (c *Cipher) Decrypt(ciphertext []byte, context string) (string, error) {
	minimumLength := 1 + c.aead.NonceSize() + c.aead.Overhead()
	if len(ciphertext) < minimumLength {
		return "", fmt.Errorf("%w: too short", ErrInvalidCiphertext)
	}
	if ciphertext[0] != Version {
		if ciphertext[0] == 0xC3 {
			return "", fmt.Errorf("%w: pgcrypto legacy ciphertext (version 195); the AppView startup conversion has not run on this database", ErrUnsupportedVersion)
		}
		return "", fmt.Errorf("%w: version %d", ErrUnsupportedVersion, ciphertext[0])
	}

	nonceEnd := 1 + c.aead.NonceSize()
	authenticatedData := append([]byte{Version}, context...)
	plaintext, err := c.aead.Open(nil, ciphertext[1:nonceEnd], ciphertext[nonceEnd:], authenticatedData)
	if err != nil {
		return "", fmt.Errorf("%w: authentication failed", ErrInvalidCiphertext)
	}
	return string(plaintext), nil
}
