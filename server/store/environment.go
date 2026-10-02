package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// TestEnvironment describes a remote machine where MD software can run: a
// CPU host, a node with a specific MPI stack, a GPU machine, etc.
type TestEnvironment struct {
	ID          int64  `gorm:"primaryKey"`
	OwnerID     int64  `gorm:"index;not null"`      // the user who manages this environment
	Name        string `gorm:"not null"`            // e.g. "cpu-node-1", "gpu-a100"
	Host        string `gorm:"not null"`            // hostname or IP, optionally host:port
	Username    string `gorm:"not null"`            // SSH login user on the host
	PrivateKey  string `gorm:"not null"`            // PEM-encoded SSH private key (login token)
	Tags        string `gorm:"not null;default:''"` // comma-separated, lowercased labels (e.g. "cpu,mpi")
	Description string `gorm:"not null;default:''"`
	Enabled     bool   `gorm:"not null;default:true"` // whether jobs may be dispatched here
	// EnvScript is the bash environment-setup script sourced before every
	// stage (build/unit/regression case) on this environment: module loads,
	// compiler exports, environment-specific paths. Empty = no script (the
	// runner warns in the task log). Not secret — returned in full by the API.
	EnvScript string `gorm:"type:text;not null;default:''"`

	// AllowedEnvVars is THIS host's comma-separated whitelist of environment
	// variable names a md-builder.yaml `variables:` value may expand on it
	// (see AllowedEnvList). The yaml is repository-side, so which of a
	// machine's environment variables it may read is the decision of whoever
	// runs that machine — the setting is per environment, not per site: two
	// hosts of the same site may expose different things.
	//
	// It is a pointer so "never configured" is distinguishable from "the
	// owner allow-listed nothing": nil (a row written before the column
	// existed) falls back to DefaultAllowedEnvVars, while a stored empty
	// string means exactly that — no host variable is expandable. New
	// environments are created with the default list stored, so the form
	// shows what it starts from. Like the other settings it is a plain
	// (non-secret) value: it names variables, never their contents.
	//
	// No `not null` and no default here, deliberately: those would erase the
	// difference this pointer exists to keep, and on a populated table the
	// default would silently turn "never configured" into "nothing allowed".
	AllowedEnvVars *string `gorm:"type:text"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// DefaultAllowedEnvVars is the minimal safe whitelist an environment starts
// with: the variables every login shell has, none of them secret. It applies
// while AllowedEnvVars is unset, and the environment form offers it back
// after the owner has narrowed the list.
const DefaultAllowedEnvVars = "HOME, USER, LOGNAME, PATH, SHELL, TMPDIR"

// AllowedEnvList returns the environment's effective whitelist as names: the
// owner's list, or DefaultAllowedEnvVars while the column is unset.
func (e *TestEnvironment) AllowedEnvList() []string {
	raw := DefaultAllowedEnvVars
	if e.AllowedEnvVars != nil {
		raw = *e.AllowedEnvVars
	}
	return ParseAllowedEnvVars(raw)
}

// ParseAllowedEnvVars splits a whitelist string into names. Names may be
// separated by commas, spaces or newlines; blanks are dropped and duplicates
// collapsed (first occurrence wins), so the caller gets a clean list in the
// order it was written. It is the one splitter for the setting — the API
// validates what it returns, the runner expands what AllowedEnvList returns.
func ParseAllowedEnvVars(raw string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, name := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	}) {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// EnvScriptName derives the on-host file name for the environment script:
// md-builder-env-<hash12>.sh, hashed from the content so edits are visible
// in logs and stale files never collide.
func (e *TestEnvironment) EnvScriptName() string {
	if strings.TrimSpace(e.EnvScript) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(e.EnvScript))
	return fmt.Sprintf("md-builder-env-%s.sh", hex.EncodeToString(sum[:])[:12])
}

// ErrMissingOwner is returned when a record requires an owning user.
var ErrMissingOwner = errors.New("store: environment requires an owner")

// BeforeSave validates required fields and normalizes tags.
func (e *TestEnvironment) BeforeSave(tx *gorm.DB) error {
	if e.OwnerID == 0 {
		return ErrMissingOwner
	}
	e.Tags = NormalizeTags(e.Tags)
	return nil
}

// NormalizeTags cleans a raw tag list: split on commas or spaces, trim,
// lowercase, drop empties and duplicates, join back with commas.
func NormalizeTags(raw string) string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return strings.Join(out, ",")
}

// TagList splits the stored tag list into individual tags. An environment
// without tags yields an empty slice (not nil) so JSON encoding produces
// [] rather than null.
func (e *TestEnvironment) TagList() []string {
	if e.Tags == "" {
		return []string{}
	}
	return strings.Split(e.Tags, ",")
}

// --- TestEnvironment queries ---

// CreateEnvironment inserts a new environment record.
func (s *Store) CreateEnvironment(env *TestEnvironment) error {
	return s.DB.Create(env).Error
}

// GetEnvironment loads an environment by id. Reads are not owner-scoped:
// every signed-in user sees the whole site-wide pool (the environment list is
// read-only for environments they do not own), and dispatch draws on it.
// Whether the caller may *change* what it read is the API layer's call.
func (s *Store) GetEnvironment(id int64) (*TestEnvironment, error) {
	var env TestEnvironment
	if err := s.DB.First(&env, id).Error; err != nil {
		return nil, err
	}
	return &env, nil
}

// UpdateEnvironment saves changes to an existing environment.
func (s *Store) UpdateEnvironment(env *TestEnvironment) error {
	return s.DB.Save(env).Error
}

// SetEnvironmentEnabled flips the enabled flag of an environment and returns
// the updated record.
func (s *Store) SetEnvironmentEnabled(id int64, enabled bool) (*TestEnvironment, error) {
	env, err := s.GetEnvironment(id)
	if err != nil {
		return nil, err
	}
	if err := s.DB.Model(env).Update("enabled", enabled).Error; err != nil {
		return nil, err
	}
	env.Enabled = enabled
	return env, nil
}

// DeleteEnvironment removes an environment, together with its test runs, case
// results and tasks (the dashboard shows site-wide history, so dangling rows
// would otherwise survive the environment).
func (s *Store) DeleteEnvironment(id int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var runIDs []int64
		if err := tx.Model(&TestRun{}).Where("environment_id = ?", id).
			Pluck("id", &runIDs).Error; err != nil {
			return err
		}
		if len(runIDs) > 0 {
			if err := deleteArtifactsByRunIDs(tx, runIDs); err != nil {
				return err
			}
			if err := tx.Where("id IN ?", runIDs).Delete(&TestRun{}).Error; err != nil {
				return err
			}
		}
		var taskIDs []int64
		if err := tx.Model(&Task{}).Where("environment_id = ?", id).
			Pluck("id", &taskIDs).Error; err != nil {
			return err
		}
		if len(taskIDs) > 0 {
			if err := tx.Where("id IN ?", taskIDs).Delete(&Task{}).Error; err != nil {
				return err
			}
		}
		return tx.Where("id = ?", id).Delete(&TestEnvironment{}).Error
	})
}

// ListAllEnvironments returns every environment on the site, name-ordered.
// The environment list and the dashboard matrix are site-wide views: an
// environment is a machine the site's tests may run on, not a private row, so
// neither is owner-scoped.
func (s *Store) ListAllEnvironments() ([]TestEnvironment, error) {
	var envs []TestEnvironment
	if err := s.DB.Order("name ASC, id ASC").Find(&envs).Error; err != nil {
		return nil, err
	}
	return envs, nil
}

// ListEnabledEnvironments returns every enabled environment, name-ordered.
// Job dispatch only considers these.
func (s *Store) ListEnabledEnvironments() ([]TestEnvironment, error) {
	var envs []TestEnvironment
	if err := s.DB.Where("enabled = ?", true).Order("name ASC, id ASC").Find(&envs).Error; err != nil {
		return nil, err
	}
	return envs, nil
}
