package auth

import "testing"

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "" || hash == "s3cret" {
		t.Fatal("hash must be non-empty and not equal to plaintext")
	}
	if err := CheckPassword(hash, "s3cret"); err != nil {
		t.Fatalf("check correct password: %v", err)
	}
	if err := CheckPassword(hash, "wrong"); err == nil {
		t.Fatal("expected mismatch for wrong password")
	}
}

func TestNewToken(t *testing.T) {
	tok1, err := NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if len(tok1) != 64 {
		t.Fatalf("expected 64-char hex token, got %d", len(tok1))
	}
	tok2, _ := NewToken()
	if tok1 == tok2 {
		t.Fatal("tokens should be unique")
	}
}
