package store

import (
	"encoding/hex"
	"testing"
)

func TestSiteConfigDefaults(t *testing.T) {
	s := newTestStore(t)

	cfg, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("GetSiteConfig: %v", err)
	}
	if cfg.ID != 1 {
		t.Fatalf("expected singleton ID 1, got %d", cfg.ID)
	}
	if cfg.CodeRepo != "" {
		t.Fatalf("expected empty defaults, got %+v", cfg)
	}
	if cfg.AccessToken != "" {
		t.Fatalf("expected empty token, got %+v", cfg)
	}
}

func TestSiteConfigSaveLoad(t *testing.T) {
	s := newTestStore(t)

	cfg := &SiteConfig{
		CodeRepo:    "https://gitlab.com/group/code",
		AccessToken: "glpat-xxxxxxxxxxxx",
		Timezone:    "Asia/Shanghai",
	}
	if err := s.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("SaveSiteConfig: %v", err)
	}

	got, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("GetSiteConfig: %v", err)
	}
	if got.CodeRepo != cfg.CodeRepo {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.AccessToken != cfg.AccessToken || got.Timezone != cfg.Timezone {
		t.Fatalf("token/timezone round-trip mismatch: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("expected UpdatedAt to be set")
	}

	// Save again (update path) with new values.
	cfg.CodeRepo = "https://gitlab.com/group/code2"
	if err := s.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("SaveSiteConfig update: %v", err)
	}
	got, err = s.GetSiteConfig()
	if err != nil {
		t.Fatalf("GetSiteConfig after update: %v", err)
	}
	if got.CodeRepo != "https://gitlab.com/group/code2" {
		t.Fatalf("expected updated repo, got %q", got.CodeRepo)
	}

	// Still exactly one row.
	var count int64
	if err := s.DB.Model(&SiteConfig{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

// TestSiteConfigWebhookToken covers the webhook secret's lifecycle: it comes
// with the configuration row, survives updates, and is regenerated when a
// row has none (the upgrade path from a database written before the column
// existed).
func TestSiteConfigWebhookToken(t *testing.T) {
	s := newTestStore(t)

	// Generated with the row: a config never exists without one.
	cfg, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("GetSiteConfig: %v", err)
	}
	if len(cfg.WebhookToken) != 64 {
		t.Fatalf("expected a 64-char token, got %q", cfg.WebhookToken)
	}
	if _, err := hex.DecodeString(cfg.WebhookToken); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}

	// Stable: reading does not rotate it.
	again, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("GetSiteConfig again: %v", err)
	}
	if again.WebhookToken != cfg.WebhookToken {
		t.Fatalf("token changed between reads: %q -> %q", cfg.WebhookToken, again.WebhookToken)
	}

	// A row saved without a token (a cfg built from scratch, or a row from
	// before the column existed) gets a fresh one on load, and only the
	// token column is written.
	if err := s.SaveSiteConfig(&SiteConfig{ID: 1, CodeRepo: "https://gitlab.com/g/code"}); err != nil {
		t.Fatalf("SaveSiteConfig: %v", err)
	}
	healed, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("GetSiteConfig after clearing: %v", err)
	}
	if healed.WebhookToken == "" || healed.WebhookToken == cfg.WebhookToken {
		t.Fatalf("expected a fresh token, got %q", healed.WebhookToken)
	}
	if healed.CodeRepo != "https://gitlab.com/g/code" {
		t.Fatalf("healing clobbered the rest of the row: %+v", healed)
	}

	// An explicitly saved token is kept as is (this is what the rotate
	// handler writes).
	if err := s.SaveSiteConfig(&SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/g/code", WebhookToken: "chosen-token"}); err != nil {
		t.Fatalf("SaveSiteConfig with token: %v", err)
	}
	kept, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("GetSiteConfig with token: %v", err)
	}
	if kept.WebhookToken != "chosen-token" {
		t.Fatalf("token not kept: %q", kept.WebhookToken)
	}

	// Two tokens never collide.
	first, err := NewWebhookToken()
	if err != nil {
		t.Fatalf("NewWebhookToken: %v", err)
	}
	second, err := NewWebhookToken()
	if err != nil {
		t.Fatalf("NewWebhookToken: %v", err)
	}
	if first == second {
		t.Fatalf("two generated tokens are identical: %q", first)
	}
}
