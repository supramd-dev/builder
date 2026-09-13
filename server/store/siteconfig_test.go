package store

import "testing"

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
