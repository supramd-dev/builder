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
// they are set is reported, never the values themselves. The webhook token
// is the one exception — it has to be copied into GitLab by hand, so it is
// returned in full, and only to an administrator (everybody else just learns
// that it is set).
type siteConfigJSON struct {
	CodeRepo       string `json:"codeRepo"`
	AccessTokenSet bool   `json:"accessTokenSet"`
	Timezone       string `json:"timezone"` // IANA name, "" = browser local
	SecretTokenSet bool   `json:"secretTokenSet"`
	WebhookToken   string `json:"webhookToken"` // administrators only
	UpdatedAt      string `json:"updatedAt"`

	// The GitLab sign-in integration. Whether it is on, and whether a client
	// secret is stored, are plain booleans that leak nothing, so everybody
	// sees them; the instance address and the application id are part of the
	// credentials pair an administrator copies into GitLab, so they are
	// reported to administrators only, like the webhook token.
	GitLabLoginEnabled    bool   `json:"gitlabLoginEnabled"`
	GitLabClientSecretSet bool   `json:"gitlabClientSecretSet"`
	GitLabURL             string `json:"gitlabUrl"`      // administrators only
	GitLabClientID        string `json:"gitlabClientId"` // administrators only
	// GitLabRedirectURI is the callback URL to register on the GitLab
	// application. It is derived from server.publicURL, and reported so an
	// administrator can copy the exact value instead of assembling it.
	GitLabRedirectURI string `json:"gitlabRedirectUri"` // administrators only
}

// siteConfigInput is the request body for updating the configuration. The
// tokens follow the environment private-key convention: an empty value
// keeps the stored one; the explicit Clear flag removes it.
//
// The GitLab fields are administrators-only (see updateSiteConfig). Three of
// them are pointers so that "the field was absent" is distinguishable from
// "set it to empty" — with a plain string the two are identical, and a
// request that only flips the switch ({"gitlabLoginEnabled": false}) would
// silently erase the credentials it never mentioned. Absent = keep, empty =
// clear (and clearing a field the integration needs is refused below).
// GitLabLoginEnabled is a pointer for the same reason, which is also what
// keeps a non-administrator's request from being mistaken for one that tries
// to change it.
type siteConfigInput struct {
	CodeRepo         string `json:"codeRepo"`
	AccessToken      string `json:"accessToken"` // empty = keep current
	Timezone         string `json:"timezone"`    // IANA name, "" = browser local
	SecretToken      string `json:"secretToken"` // empty = keep current
	ClearAccessToken bool   `json:"clearAccessToken"`
	ClearSecretToken bool   `json:"clearSecretToken"`

	GitLabURL               *string `json:"gitlabUrl"`      // absent = keep current
	GitLabClientID          *string `json:"gitlabClientId"` // absent = keep current
	GitLabClientSecret      string  `json:"gitlabClientSecret"`
	ClearGitLabClientSecret bool    `json:"clearGitLabClientSecret"`
	GitLabLoginEnabled      *bool   `json:"gitlabLoginEnabled"`
}

// touchesGitLab reports whether the request tries to change any part of the
// GitLab sign-in configuration. The update endpoint is shared by the
// Repository tab (everyone) and the GitLab tab (administrators only), so this
// is what decides whether the caller needs to be an administrator.
func (in *siteConfigInput) touchesGitLab() bool {
	return in.GitLabURL != nil ||
		in.GitLabClientID != nil ||
		in.GitLabClientSecret != "" ||
		in.ClearGitLabClientSecret ||
		in.GitLabLoginEnabled != nil
}

// handleSiteConfig routes GET/PUT /api/site-config. Any logged-in user may
// read or update the configuration; rotating the webhook secret has its own
// administrators-only endpoint, and only administrators are shown its value.
func (s *Server) handleSiteConfig(w http.ResponseWriter, r *http.Request, user *store.User) {
	switch r.Method {
	case http.MethodGet:
		s.getSiteConfig(w, r, user)
	case http.MethodPut, http.MethodPatch:
		s.updateSiteConfig(w, r, user)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleWebhookTokenRotate replaces the webhook secret (POST
// /api/site-config/webhook-token) and answers with the whole configuration,
// the new token included. Rotating is a separate call rather than a flag on
// the update above: it must not carry — and so cannot clobber — the rest of
// the configuration, and it has to work on a site whose code repository is
// not filled in yet. The caller is an administrator: requireAdmin wraps this
// handler.
func (s *Server) handleWebhookTokenRotate(w http.ResponseWriter, r *http.Request, user *store.User) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	token, err := store.NewWebhookToken()
	if err != nil {
		log.Printf("generate webhook token: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		log.Printf("get site config for webhook token rotation: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	cfg.WebhookToken = token
	if err := s.Store.SaveSiteConfig(cfg); err != nil {
		log.Printf("save rotated webhook token: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Logged by who, never the token itself.
	log.Printf("webhook token rotated by %q (id %d)", user.Username, user.ID)
	writeJSON(w, http.StatusOK, s.toSiteConfigJSON(cfg, user))
}

func (s *Server) getSiteConfig(w http.ResponseWriter, r *http.Request, user *store.User) {
	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		log.Printf("get site config: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.toSiteConfigJSON(cfg, user))
}

func (s *Server) updateSiteConfig(w http.ResponseWriter, r *http.Request, user *store.User) {
	var in siteConfigInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if msg := validateSiteConfigInput(&in); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// The GitLab sign-in configuration carries an OAuth client secret and
	// decides who may register, so only an administrator may touch it. The
	// rest of the endpoint stays open to every logged-in user, as before.
	if in.touchesGitLab() && !user.IsAdmin() {
		log.Printf("site config update denied: %q (id %d) is not an administrator and tried to change the GitLab integration",
			user.Username, user.ID)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator permission required"})
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

	// GitLab sign-in: only an administrator reaches here with these set.
	// A nil address or id means the request said nothing about it, so the
	// stored value stays — flipping the switch must not erase the credentials
	// the request never mentioned. The client secret follows the same
	// write-only convention as the two tokens above.
	if in.touchesGitLab() {
		if in.GitLabURL != nil {
			cfg.GitLabURL = strings.TrimSpace(*in.GitLabURL)
		}
		if in.GitLabClientID != nil {
			cfg.GitLabClientID = strings.TrimSpace(*in.GitLabClientID)
		}
		if in.ClearGitLabClientSecret {
			cfg.GitLabClientSecret = ""
		} else if secret := strings.TrimSpace(in.GitLabClientSecret); secret != "" {
			cfg.GitLabClientSecret = secret
		}
		if in.GitLabLoginEnabled != nil {
			cfg.GitLabLoginEnabled = *in.GitLabLoginEnabled
		}
	}

	// Enabling the integration with a piece missing would advertise a login
	// button that cannot work, so refuse the combination instead. Checked
	// after the merge, so it sees the values the row will actually hold (a
	// blank secret in the request means "keep the stored one").
	if cfg.GitLabLoginEnabled {
		if msg := validateGitLabReady(cfg); msg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
			return
		}
	}

	if err := s.Store.SaveSiteConfig(cfg); err != nil {
		log.Printf("save site config: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.toSiteConfigJSON(cfg, user))
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

// validateGitLabReady reports why the GitLab sign-in integration cannot be
// switched on with the given configuration, or "" when it is complete.
func validateGitLabReady(cfg *store.SiteConfig) string {
	if strings.TrimSpace(cfg.GitLabURL) == "" {
		return "set the GitLab site address before enabling GitLab sign-in"
	}
	// A bare host ("gitlab.example.com", no scheme) would build request URLs
	// with no scheme at all, which fail at the first call with an error that
	// says nothing about the cause. Refuse the shape here instead.
	if !isAbsoluteHTTPURL(cfg.GitLabURL) {
		return "the GitLab site address must be a full URL including http:// or https://"
	}
	if strings.TrimSpace(cfg.GitLabClientID) == "" {
		return "set the GitLab application ID before enabling GitLab sign-in"
	}
	if strings.TrimSpace(cfg.GitLabClientSecret) == "" {
		return "set the GitLab application secret before enabling GitLab sign-in"
	}
	return ""
}

func (s *Server) toSiteConfigJSON(cfg *store.SiteConfig, user *store.User) siteConfigJSON {
	out := siteConfigJSON{
		CodeRepo:       cfg.CodeRepo,
		AccessTokenSet: strings.TrimSpace(cfg.AccessToken) != "",
		Timezone:       cfg.Timezone,
		SecretTokenSet: strings.TrimSpace(cfg.SecretToken) != "",
		UpdatedAt:      cfg.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),

		GitLabLoginEnabled:    cfg.GitLabLoginEnabled,
		GitLabClientSecretSet: strings.TrimSpace(cfg.GitLabClientSecret) != "",
	}
	// The webhook secret is readable, but only by the administrators who
	// configure the GitLab side of it. The GitLab instance address,
	// application id and callback URL are what those administrators have to
	// copy into GitLab, so they follow the same rule. The callback URL is
	// reported only when the site address is configured — without it there
	// is no absolute URL to register.
	if user.IsAdmin() {
		out.WebhookToken = cfg.WebhookToken
		out.GitLabURL = cfg.GitLabURL
		out.GitLabClientID = cfg.GitLabClientID
		if s.PublicURL != "" {
			out.GitLabRedirectURI = s.gitlabRedirectURI()
		}
	}
	return out
}
