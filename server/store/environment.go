package store

import (
	"errors"
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
	CreatedAt   time.Time
	UpdatedAt   time.Time
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

// TagList splits the stored tag list into individual tags.
func (e *TestEnvironment) TagList() []string {
	if e.Tags == "" {
		return nil
	}
	return strings.Split(e.Tags, ",")
}

// --- TestEnvironment queries ---

// CreateEnvironment inserts a new environment record.
func (s *Store) CreateEnvironment(env *TestEnvironment) error {
	return s.DB.Create(env).Error
}

// ListEnvironments returns all environments owned by the given user, newest first.
func (s *Store) ListEnvironments(ownerID int64) ([]TestEnvironment, error) {
	var envs []TestEnvironment
	if err := s.DB.Where("owner_id = ?", ownerID).Order("updated_at DESC").Find(&envs).Error; err != nil {
		return nil, err
	}
	return envs, nil
}

// GetEnvironment loads a single environment, ensuring it belongs to ownerID.
func (s *Store) GetEnvironment(ownerID, id int64) (*TestEnvironment, error) {
	var env TestEnvironment
	if err := s.DB.Where("id = ? AND owner_id = ?", id, ownerID).First(&env).Error; err != nil {
		return nil, err
	}
	return &env, nil
}

// GetEnvironmentAny loads an environment by id regardless of ownership —
// used by site-wide views (dashboard) and result reporting.
func (s *Store) GetEnvironmentAny(id int64) (*TestEnvironment, error) {
	var env TestEnvironment
	if err := s.DB.First(&env, id).Error; err != nil {
		return nil, err
	}
	return &env, nil
}

// UpdateEnvironment saves changes to an existing environment owned by ownerID.
func (s *Store) UpdateEnvironment(env *TestEnvironment) error {
	return s.DB.Save(env).Error
}

// SetEnvironmentEnabled flips the enabled flag of an environment owned by
// ownerID and returns the updated record.
func (s *Store) SetEnvironmentEnabled(ownerID, id int64, enabled bool) (*TestEnvironment, error) {
	env, err := s.GetEnvironment(ownerID, id)
	if err != nil {
		return nil, err
	}
	if err := s.DB.Model(env).Update("enabled", enabled).Error; err != nil {
		return nil, err
	}
	env.Enabled = enabled
	return env, nil
}

// DeleteEnvironment removes an environment owned by ownerID, together with
// its test runs and case results (the dashboard shows site-wide history, so
// dangling rows would otherwise survive the environment).
func (s *Store) DeleteEnvironment(ownerID, id int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var runIDs []int64
		if err := tx.Model(&TestRun{}).Where("environment_id = ?", id).
			Pluck("id", &runIDs).Error; err != nil {
			return err
		}
		if len(runIDs) > 0 {
			if err := tx.Where("test_run_id IN ?", runIDs).Delete(&TestCaseResult{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", runIDs).Delete(&TestRun{}).Error; err != nil {
				return err
			}
		}
		return tx.Where("id = ? AND owner_id = ?", id, ownerID).Delete(&TestEnvironment{}).Error
	})
}

// ListAllEnvironments returns every environment on the site, name-ordered.
// The dashboard matrix is a site-wide view, so it is not owner-scoped.
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
