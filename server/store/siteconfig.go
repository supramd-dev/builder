package store

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

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
	// deliberately not tied to a specific problem domain. Test inputs are
	// expected to live inside the code repository itself (or to be fetched
	// by it), so there is no separate test-input repository.
	CodeRepo string

	// AccessToken is a GitLab Project Access Token (or group/personal
	// access token) with read_repository scope, used for every git
	// operation (clone, ref resolution, md-builder.yaml fetch) over
	// HTTPS. Empty means "public repository, no authentication". It is
	// display-only — server internals treat it as an opaque secret.
	AccessToken string

	// Timezone is the IANA time zone name every timestamp is displayed in
	// (e.g. "Asia/Shanghai"). Empty means "the viewer's browser local
	// zone". The name is validated against the time/tzdata database; it is
	// display-only — server internals always work in UTC.
	Timezone string

	// SecretToken is a site-wide secret exported to every stage command as
	// MD_SECRET_TOKEN (build / unit / regression case scripts in
	// md-builder.yaml). It lets commands authenticate against internal
	// services (package mirrors, artifact stores, licensed software
	// servers) without hardcoding credentials in the repository. Like
	// AccessToken it is write-only: never returned by the API, and the
	// runner redacts it from task logs. Empty = not configured (the
	// variable is unset).
	SecretToken string

	// WebhookToken is the shared secret the webhook endpoint verifies: GitLab
	// must echo it back in the X-Gitlab-Token header, and an event without a
	// matching token is rejected. It is deliberately separate from
	// SecretToken, which travels the other way — SecretToken is handed to the
	// build scripts as MD_SECRET_TOKEN, this one only authenticates inbound
	// webhook calls and is never exported to a stage command.
	//
	// Unlike the two tokens above it is readable through the API (to
	// administrators), because the administrator has to copy it into the
	// GitLab webhook form. GetSiteConfig generates one whenever the row has
	// none, so it is set from the moment the site config first exists.
	WebhookToken string

	// GitLabURL is the base address of the GitLab instance users sign in
	// against ("https://gitlab.com", or a self-hosted instance). It is the
	// instance the OAuth application below belongs to, and is independent of
	// CodeRepo: the repository under test may live somewhere else entirely.
	GitLabURL string

	// GitLabClientID and GitLabClientSecret are the OAuth application
	// ("Application ID" and "Secret" in GitLab's Admin Area → Applications)
	// the sign-in integration authenticates with. The id is readable through
	// the API to an administrator, who has to be able to compare it against
	// the GitLab form; the secret is write-only, exactly like AccessToken
	// and SecretToken — never returned, and redacted from errors and logs.
	GitLabClientID     string
	GitLabClientSecret string

	// GitLabLoginEnabled is the administrator's switch for the whole
	// integration. While it is false the sign-in routes answer "not
	// configured" and the login page shows no GitLab button, so an
	// accidentally half-filled configuration cannot expose a login path.
	GitLabLoginEnabled bool

	UpdatedAt time.Time
}

// NewWebhookToken returns a fresh webhook shared secret (32 random bytes as
// hex). It is a separate helper from auth.NewToken so this package does not
// depend on the auth layer: a session token and a webhook token are the same
// shape but unrelated.
func NewWebhookToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GetSiteConfig loads the singleton configuration row, creating a default
// one when it does not exist yet. A row without a webhook token (a fresh
// install, or one written before the column existed) gets one here, so the
// webhook endpoint always has something to verify against.
func (s *Store) GetSiteConfig() (*SiteConfig, error) {
	var cfg SiteConfig
	err := s.DB.First(&cfg, 1).Error
	if err == nil {
		return s.ensureWebhookToken(&cfg)
	}
	if err.Error() != "record not found" {
		return nil, err
	}
	// First access: create the default row, webhook token included.
	token, err := NewWebhookToken()
	if err != nil {
		return nil, err
	}
	cfg = SiteConfig{ID: 1, WebhookToken: token}
	if err := s.DB.Create(&cfg).Error; err != nil {
		// A concurrent request may have created it first; load then.
		var existing SiteConfig
		if lerr := s.DB.First(&existing, 1).Error; lerr == nil {
			return s.ensureWebhookToken(&existing)
		}
		return nil, err
	}
	return &cfg, nil
}

// ensureWebhookToken fills in a missing webhook token and returns the row. It
// writes the single column rather than saving the whole row, so a concurrent
// update of the other fields is not clobbered. Callers that save a
// SiteConfig built from scratch (with no token) are healed by the next load.
func (s *Store) ensureWebhookToken(cfg *SiteConfig) (*SiteConfig, error) {
	if strings.TrimSpace(cfg.WebhookToken) != "" {
		return cfg, nil
	}
	token, err := NewWebhookToken()
	if err != nil {
		return nil, err
	}
	if err := s.DB.Model(&SiteConfig{}).Where("id = ?", cfg.ID).
		Update("webhook_token", token).Error; err != nil {
		return nil, err
	}
	cfg.WebhookToken = token
	return cfg, nil
}

// SaveSiteConfig upserts the singleton configuration row. It writes every
// column, so a cfg built from scratch (rather than loaded first) carries no
// webhook token and clears the stored one — the next GetSiteConfig generates
// a replacement.
func (s *Store) SaveSiteConfig(cfg *SiteConfig) error {
	cfg.ID = 1
	// GetSiteConfig ensures the row exists; save over it either way.
	if _, err := s.GetSiteConfig(); err != nil {
		return err
	}
	return s.DB.Save(cfg).Error
}
