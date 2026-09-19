package auth

import (
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	cases := []struct {
		name     string
		username string
		wantErr  bool
	}{
		{"plain", "alice", false},
		{"dots and dashes", "a.b-c_d", false},
		{"at the limit", strings.Repeat("a", MaxUsernameLength), false},
		{"empty", "", true},
		{"too long", strings.Repeat("a", MaxUsernameLength+1), true},
		{"inner space", "al ice", true},
		{"tab", "al\tice", true},
		{"newline", "alice\nadmin", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := ValidateUsername(tc.username)
			if tc.wantErr != (msg != "") {
				t.Fatalf("ValidateUsername(%q) = %q, wantErr %v", tc.username, msg, tc.wantErr)
			}
		})
	}
}

func TestValidateEmail(t *testing.T) {
	cases := []struct {
		name    string
		email   string
		wantErr bool
	}{
		{"plain", "alice@example.com", false},
		{"at the limit", strings.Repeat("a", MaxEmailLength-2) + "@x", false},
		{"empty", "", true},
		{"no at", "alice.example.com", true},
		{"no local part", "@example.com", true},
		{"no domain", "alice@", true},
		{"two at signs", "alice@@example.com", true},
		{"space", "alice @example.com", true},
		{"too long", strings.Repeat("a", MaxEmailLength) + "@example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := ValidateEmail(tc.email)
			if tc.wantErr != (msg != "") {
				t.Fatalf("ValidateEmail(%q) = %q, wantErr %v", tc.email, msg, tc.wantErr)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"at the minimum", "12345678", false},
		{"long", "a-perfectly-fine-passphrase", false},
		{"at the byte limit", strings.Repeat("a", MaxPasswordBytes), false},
		{"empty", "", true},
		{"too short", "1234567", true},
		// 3 runes but 9 bytes: the rune count is what decides the minimum.
		{"short but multi-byte", "密码密", true},
		{"eight multi-byte runes", "密码密码密码密码", false},
		{"past the bcrypt limit", strings.Repeat("a", MaxPasswordBytes+1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := ValidatePassword(tc.password)
			if tc.wantErr != (msg != "") {
				t.Fatalf("ValidatePassword(%q) = %q, wantErr %v", tc.password, msg, tc.wantErr)
			}
		})
	}
}

// TestValidatedPasswordHashes keeps the rules and bcrypt in agreement: a
// password the validator accepts must hash, and one past the byte limit must
// not be silently truncated.
func TestValidatedPasswordHashes(t *testing.T) {
	if msg := ValidatePassword("12345678"); msg != "" {
		t.Fatalf("unexpected rejection: %s", msg)
	}
	if _, err := HashPassword("12345678"); err != nil {
		t.Fatalf("hash accepted password: %v", err)
	}
	if msg := ValidatePassword(strings.Repeat("a", MaxPasswordBytes+1)); msg == "" {
		t.Fatal("expected a password past the bcrypt limit to be rejected")
	}
}
