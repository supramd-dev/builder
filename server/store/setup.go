package store

import (
	"errors"

	"gorm.io/gorm"
)

// ErrSetupDone is returned by CompleteSetup when the site already has an
// account: the first-run setup window is closed for good.
var ErrSetupDone = errors.New("the site is already set up")

// SetupRequired reports whether the site still needs its first-run setup: it
// does while no account exists at all. The moment one does — created by
// CompleteSetup, by adduser or by seed — the setup page and its endpoints are
// closed for good.
//
// "No account at all" is the gate on purpose. The setup endpoint cannot
// authenticate its caller (there is no session, and no administrator to create
// one), so the only thing that makes it safe to expose is that it has nothing
// left to do once somebody can log in. Gating on "no administrator" instead
// would leave the endpoint open on a site that already has regular users, and
// the first anonymous caller could claim it.
func (s *Store) SetupRequired() (bool, error) {
	var count int64
	if err := s.DB.Model(&User{}).Count(&count).Error; err != nil {
		return false, err
	}
	return count == 0, nil
}

// CompleteSetup creates the site's first account and stores the code
// repository with its access token, in one transaction. It returns
// ErrSetupDone — having written nothing — as soon as an account exists.
//
// The account is built by the caller, password already hashed; the role is
// set here rather than taken from u, so the first account of a site is an
// administrator and no caller can make it anything else. This is the one
// place a role is not decided by the CLI: it cannot be, since the site has no
// administrator to run the CLI as yet.
func (s *Store) CompleteSetup(u *User, codeRepo, accessToken string) error {
	// The configuration row is ensured outside the transaction: GetSiteConfig
	// is idempotent, and it generates the webhook token on first access.
	// Doing it here means the update below writes over an existing row rather
	// than replacing one built from scratch — which would clear the token.
	if _, err := s.GetSiteConfig(); err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		// Write the configuration first: it locks the singleton row, so two
		// setup requests racing each other are serialized here instead of
		// both counting zero accounts below. A refusal rolls this write back.
		updates := map[string]any{"code_repo": codeRepo}
		if accessToken != "" {
			updates["access_token"] = accessToken
		}
		if err := tx.Model(&SiteConfig{}).Where("id = ?", 1).Updates(updates).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&User{}).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrSetupDone
		}
		u.Role = RoleAdmin
		return tx.Create(u).Error
	})
}
