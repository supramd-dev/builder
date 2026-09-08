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
	if cfg.CodeRepo != "" || cfg.TestInputRepo != "" || cfg.TestRepoRef != "" {
		t.Fatalf("expected empty defaults, got %+v", cfg)
	}
}

func TestSiteConfigSaveLoad(t *testing.T) {
	s := newTestStore(t)

	cfg := &SiteConfig{
		CodeRepo:      "https://gitlab.com/group/code",
		TestInputRepo: "https://gitlab.com/group/test-inputs",
		TestRepoRef:   "main",
	}
	if err := s.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("SaveSiteConfig: %v", err)
	}

	got, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("GetSiteConfig: %v", err)
	}
	if got.CodeRepo != cfg.CodeRepo ||
		got.TestInputRepo != cfg.TestInputRepo ||
		got.TestRepoRef != cfg.TestRepoRef {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("expected UpdatedAt to be set")
	}

	// Save again (update path) with new values.
	cfg.TestRepoRef = "abc123def"
	if err := s.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("SaveSiteConfig update: %v", err)
	}
	got, err = s.GetSiteConfig()
	if err != nil {
		t.Fatalf("GetSiteConfig after update: %v", err)
	}
	if got.TestRepoRef != "abc123def" {
		t.Fatalf("expected updated ref, got %q", got.TestRepoRef)
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
