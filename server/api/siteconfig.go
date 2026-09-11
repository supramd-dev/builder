package api

import (
	"log"
	"net/http"
	"strings"

	"md-builder/server/store"
)

// siteConfigJSON is the wire representation of the site configuration. The
// deploy key and token are write-only secrets: only whether they are set is
// reported, never the values themselves.
type siteConfigJSON struct {
	CodeRepo        string `json:"codeRepo"`
	TestInputRepo   string `json:"testInputRepo"`
	TestRepoRef     string `json:"testRepoRef"`
	DeployKeySet    bool   `json:"deployKeySet"`
	DeployTokenSet  bool   `json:"deployTokenSet"`
	DeployTokenUser string `json:"deployTokenUser"`
	UpdatedAt       string `json:"updatedAt"`
}

// siteConfigInput is the request body for updating the configuration. The
// secret fields follow the environment private-key convention: an empty
// value keeps the stored one; the explicit Clear* flags remove it.
type siteConfigInput struct {
	CodeRepo         string `json:"codeRepo"`
	TestInputRepo    string `json:"testInputRepo"`
	TestRepoRef      string `json:"testRepoRef"`
	DeployKey        string `json:"deployKey"`       // empty = keep current
	DeployToken      string `json:"deployToken"`     // empty = keep current
	DeployTokenUser  string `json:"deployTokenUser"` // not a secret, replaced as given
	ClearDeployKey   bool   `json:"clearDeployKey"`
	ClearDeployToken bool   `json:"clearDeployToken"`
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
	cfg.TestInputRepo = strings.TrimSpace(in.TestInputRepo)
	cfg.TestRepoRef = strings.TrimSpace(in.TestRepoRef)
	// Credentials: empty = keep, Clear* = remove, otherwise replace. The
	// deploy key keeps its trailing newline: OpenSSH-format keys are
	// rejected by the ssh CLI ("invalid format") without one.
	if in.ClearDeployKey {
		cfg.DeployKey = ""
	} else if key := normalizePEMKey(in.DeployKey); key != "" {
		cfg.DeployKey = key
	}
	if in.ClearDeployToken {
		cfg.DeployToken = ""
	} else if tok := strings.TrimSpace(in.DeployToken); tok != "" {
		cfg.DeployToken = tok
	}
	cfg.DeployTokenUser = strings.TrimSpace(in.DeployTokenUser)
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
	if strings.TrimSpace(in.TestInputRepo) == "" {
		return "test input repository is required"
	}
	if strings.TrimSpace(in.TestRepoRef) == "" {
		return "branch or commit id is required"
	}
	if key := strings.TrimSpace(in.DeployKey); key != "" && !isPEMKey(key) {
		return "deploy key must be a PEM-encoded SSH key"
	}
	return ""
}

func toSiteConfigJSON(cfg *store.SiteConfig) siteConfigJSON {
	return siteConfigJSON{
		CodeRepo:        cfg.CodeRepo,
		TestInputRepo:   cfg.TestInputRepo,
		TestRepoRef:     cfg.TestRepoRef,
		DeployKeySet:    strings.TrimSpace(cfg.DeployKey) != "",
		DeployTokenSet:  strings.TrimSpace(cfg.DeployToken) != "",
		DeployTokenUser: cfg.DeployTokenUser,
		UpdatedAt:       cfg.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}
