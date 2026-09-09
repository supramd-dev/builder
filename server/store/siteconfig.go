package store

import "time"

// SiteConfig holds the site-wide repository configuration. There is exactly
// one row (ID = 1); every logged-in user may read it, and updates replace
// the whole row.
//
// Repositories must be hosted on GitLab (the only platform currently
// supported); this is enforced at the API layer, not here.
type SiteConfig struct {
	ID int64 `gorm:"primaryKey"`

	// CodeRepo is the location of the code repository under test, e.g.
	// "https://gitlab.com/group/code" or "gitlab.com:group/code". It is
	// deliberately not tied to a specific problem domain.
	CodeRepo string

	// TestInputRepo is the location of the test input repository — the
	// inputs used to exercise the code under test.
	TestInputRepo string

	// TestRepoRef is the branch name or commit id of the test input
	// repository to run tests against.
	TestRepoRef string

	// DeployKey optionally holds a PEM-encoded SSH private key of a GitLab
	// deploy key, used to clone the repositories over SSH (and to convert
	// https repository URLs to SSH). Write-only via the API: it is never
	// returned to clients.
	DeployKey string

	// DeployToken optionally holds a GitLab deploy token (or personal/group
	// access token) used to clone the repositories over HTTPS, paired with
	// DeployTokenUser. Write-only via the API.
	DeployToken string

	// DeployTokenUser is the username GitLab issued alongside DeployToken
	// (e.g. "gitlab+deploy-token-42"). Not a secret. Empty means "oauth2",
	// the default for personal/group access tokens.
	DeployTokenUser string

	UpdatedAt time.Time
}

// GetSiteConfig loads the singleton configuration row, creating a default
// one when it does not exist yet.
func (s *Store) GetSiteConfig() (*SiteConfig, error) {
	var cfg SiteConfig
	err := s.DB.First(&cfg, 1).Error
	if err == nil {
		return &cfg, nil
	}
	if err.Error() != "record not found" {
		return nil, err
	}
	// First access: create the empty default row.
	cfg = SiteConfig{ID: 1}
	if err := s.DB.Create(&cfg).Error; err != nil {
		// A concurrent request may have created it first; load then.
		var existing SiteConfig
		if lerr := s.DB.First(&existing, 1).Error; lerr == nil {
			return &existing, nil
		}
		return nil, err
	}
	return &cfg, nil
}

// SaveSiteConfig upserts the singleton configuration row.
func (s *Store) SaveSiteConfig(cfg *SiteConfig) error {
	cfg.ID = 1
	// GetSiteConfig ensures the row exists; save over it either way.
	if _, err := s.GetSiteConfig(); err != nil {
		return err
	}
	return s.DB.Save(cfg).Error
}
