package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"md-builder/server/store"
)

// siteConfigJSON is the wire representation of the site configuration. The
// access token and the secret token are write-only secrets: only whether
// they are set is reported, never the values themselves.
type siteConfigJSON struct {
	CodeRepo        string `json:"codeRepo"`
	AccessTokenSet  bool   `json:"accessTokenSet"`
	Timezone        string `json:"timezone"` // IANA name, "" = browser local
	SecretTokenSet  bool   `json:"secretTokenSet"`
	UpdatedAt       string `json:"updatedAt"`
}

// siteConfigInput is the request body for updating the configuration. The
// tokens follow the environment private-key convention: an empty value
// keeps the stored one; the explicit Clear flag removes it.
type siteConfigInput struct {
	CodeRepo         string `json:"codeRepo"`
	AccessToken      string `json:"accessToken"`      // empty = keep current
	Timezone         string `json:"timezone"`         // IANA name, "" = browser local
	SecretToken      string `json:"secretToken"`      // empty = keep current
	ClearAccessToken bool   `json:"clearAccessToken"`
	ClearSecretToken bool   `json:"clearSecretToken"`
}

// handleSiteConfig routes GET/PUT /api/site-config. Any logged-in user may
// read or update the configuration; the user argument is injected by
// requireAuth and not otherwise needed.
func (s *Server) handleSiteConfig(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	switch r.Method {
	case http.MethodGet:
		s.getSiteConfig(w, r)
	case http.MethodPut, http.MethodPatch:
		s.updateSiteConfig(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) getSiteConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		log.Printf("get site config: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, toSiteConfigJSON(cfg))
}

func (s *Server) updateSiteConfig(w http.ResponseWriter, r *http.Request) {
	var in siteConfigInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if msg := validateSiteConfigInput(&in); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		log.Printf("get site config for update: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	cfg.CodeRepo = strings.TrimSpace(in.CodeRepo)
	// Token: empty = keep, Clear = remove, otherwise replace.
	if in.ClearAccessToken {
		cfg.AccessToken = ""
	} else if tok := strings.TrimSpace(in.AccessToken); tok != "" {
		cfg.AccessToken = tok
	}
	// Secret token: the same write-only convention.
	if in.ClearSecretToken {
		cfg.SecretToken = ""
	} else if tok := strings.TrimSpace(in.SecretToken); tok != "" {
		cfg.SecretToken = tok
	}
	cfg.Timezone = strings.TrimSpace(in.Timezone)
	if err := s.Store.SaveSiteConfig(cfg); err != nil {
		log.Printf("save site config: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, toSiteConfigJSON(cfg))
}

// validateSiteConfigInput returns a human-readable error message, or "".
// Repository locations are not host-validated here: self-hosted GitLab
// instances live on arbitrary hosts, so any URL/SSH path is accepted.
func validateSiteConfigInput(in *siteConfigInput) string {
	if strings.TrimSpace(in.CodeRepo) == "" {
		return "code repository is required"
	}
	if tz := strings.TrimSpace(in.Timezone); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return "unknown timezone " + tz + " (use an IANA name like Asia/Shanghai)"
		}
	}
	return ""
}

func toSiteConfigJSON(cfg *store.SiteConfig) siteConfigJSON {
	return siteConfigJSON{
		CodeRepo:       cfg.CodeRepo,
		AccessTokenSet: strings.TrimSpace(cfg.AccessToken) != "",
		Timezone:       cfg.Timezone,
		SecretTokenSet: strings.TrimSpace(cfg.SecretToken) != "",
		UpdatedAt:      cfg.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}
