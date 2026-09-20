package auth

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Account field limits. The username cap keeps the login form, the navbar and
// the task logs readable; the email cap is the RFC 5321 maximum.
const (
	MaxUsernameLength = 64
	MaxEmailLength    = 254

	// MinPasswordLength is the shortest accepted password. bcrypt hashes at
	// most 72 bytes and silently ignores the rest, so a longer password is
	// rejected rather than quietly truncated.
	MinPasswordLength = 8
	MaxPasswordBytes  = 72
)

// ValidateUsername returns a message describing why the username is
// unacceptable, or "" when it is fine. It is the single rule the CLI and the
// API both apply, so an account cannot be created one way and edited the
// other.
func ValidateUsername(username string) string {
	switch {
	case username == "":
		return "username is required"
	case utf8.RuneCountInString(username) > MaxUsernameLength:
		return fmt.Sprintf("username must be at most %d characters", MaxUsernameLength)
	}
	for _, r := range username {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "username must not contain spaces or control characters"
		}
	}
	return ""
}

// ValidateEmail applies the same kind of minimal sanity check: a local part, a
// single "@", a domain, no whitespace. Addresses are not verified beyond
// that — md-builder never sends mail.
func ValidateEmail(email string) string {
	switch {
	case email == "":
		return "email is required"
	case utf8.RuneCountInString(email) > MaxEmailLength:
		return fmt.Sprintf("email must be at most %d characters", MaxEmailLength)
	}
	for _, r := range email {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "email must not contain spaces or control characters"
		}
	}
	local, domain, found := strings.Cut(email, "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "email must look like user@example.com"
	}
	return ""
}

// ValidatePassword returns a message describing why the password is
// unacceptable, or "" when it is fine.
func ValidatePassword(password string) string {
	switch {
	case utf8.RuneCountInString(password) < MinPasswordLength:
		return fmt.Sprintf("password must be at least %d characters", MinPasswordLength)
	case len(password) > MaxPasswordBytes:
		return fmt.Sprintf("password must be at most %d bytes", MaxPasswordBytes)
	}
	return ""
}
