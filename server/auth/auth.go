// Package auth contains helpers for account field validation, password
// hashing and session tokens.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is the bcrypt cost factor. 12 is a reasonable default as of 2026.
const bcryptCost = 12

// sessionTTL is how long a login session stays valid.
const sessionTTL = 7 * 24 * time.Hour

// ErrEmptyToken is returned by validation helpers when the token is blank.
var ErrEmptyToken = errors.New("auth: empty token")

// HashPassword returns a bcrypt hash of the given plaintext password.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword reports whether the plaintext matches the stored bcrypt hash.
func CheckPassword(hash, plain string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
}

// NewToken returns a cryptographically random hex token (32 bytes / 64 chars).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SessionExpiry returns the expiry time for a session created at the given time.
func SessionExpiry(created time.Time) time.Time {
	return created.Add(sessionTTL)
}
