// Package store provides the persistence layer for md-builder.
//
// It uses GORM as the ORM so that schema migrations (AutoMigrate) and CRUD
// queries stay concise and dialect-agnostic. Both SQLite and PostgreSQL are
// supported; the driver is selected automatically from the DSN:
//
//   - "postgres://" / "postgresql://" -> PostgreSQL
//   - anything else                    -> a SQLite file path
//
// The SQLite driver (glebarez/sqlite) is pure Go and requires no CGO.
package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"md-builder/server/storage"
)

// The two account roles. An administrator may manage other accounts; a
// regular user may only edit their own. Roles are set by the CLI alone —
// adduser -admin — so no API path can create or promote an administrator.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User is an application user. PasswordHash holds a bcrypt hash, never a
// plaintext password.
type User struct {
	ID           int64  `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;not null"`
	Email        string `gorm:"uniqueIndex;not null"`
	PasswordHash string `gorm:"not null"`
	// Role is RoleAdmin or RoleUser. The default covers rows created before
	// the column existed, which AutoMigrate adds as regular users.
	Role string `gorm:"not null;default:user"`
	// Disabled accounts cannot log in, and their existing sessions stop
	// working.
	Disabled  bool `gorm:"not null;default:false"`
	CreatedAt time.Time
}

// IsAdmin reports whether the user may manage other accounts.
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

// UserUpdate carries the mutable fields of an account, already validated and
// with the password hashed. Role is deliberately absent: only the CLI sets a
// role, so an update can never escalate one.
type UserUpdate struct {
	Username     string
	Email        string
	PasswordHash string // "" = keep the stored hash
	Disabled     bool
}

// Session is a login session backed by a random token stored in the DB.
type Session struct {
	Token     string `gorm:"primaryKey"`
	UserID    int64  `gorm:"index;not null"`
	CreatedAt time.Time
	ExpiresAt time.Time `gorm:"not null"`
}

// ErrNotFound is returned when no row matches the query.
var ErrNotFound = gorm.ErrRecordNotFound

// Store wraps a *gorm.DB plus the object storage that holds artifact bytes
// (see objectstore.go).
type Store struct {
	DB *gorm.DB

	// objects is the artifact backend, set by WithObjects. nil means no
	// backend was configured; recording an artifact then fails rather than
	// falling back to inline storage.
	objects storage.Store
}

// Open opens the database described by dsn and runs AutoMigrate.
func Open(dsn string, opts ...Option) (*Store, error) {
	db, err := openDB(dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.migrate(); err != nil {
		sqlDB, _ := db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
		return nil, err
	}
	return s, nil
}

func openDB(dsn string) (*gorm.DB, error) {
	gormCfg := &gorm.Config{
		// Silent: ErrRecordNotFound is expected in normal flows (login miss,
		// anonymous requests), so we do not want GORM to log it as warnings.
		Logger: logger.Default.LogMode(logger.Silent),
	}
	var db *gorm.DB
	var err error
	switch {
	case isPostgres(dsn):
		db, err = gorm.Open(postgres.Open(dsn), gormCfg)
	default:
		// WAL + a busy timeout: the runner workers write concurrently with
		// the API server; without these, concurrent writers fail with
		// SQLITE_BUSY ("database is locked") instead of waiting briefly.
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
		db, err = gorm.Open(sqlite.Open(dsn), gormCfg)
	}
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return db, nil
}

// migrate creates/updates the schema via GORM AutoMigrate. The former
// `jobs` table is intentionally no longer migrated (superseded by `tasks`;
// stale rows in old databases are harmless).
func (s *Store) migrate() error {
	// commits (repo, sha) became a plain index (manual triggers may record
	// the same SHA more than once); AutoMigrate never drops the old unique
	// index, so do it by name when it still exists.
	if s.DB.Migrator().HasIndex(&Commit{}, "idx_commits_repo_sha") {
		if err := s.DB.Migrator().DropIndex(&Commit{}, "idx_commits_repo_sha"); err != nil {
			return fmt.Errorf("drop old commits unique index: %w", err)
		}
	}
	if err := s.DB.AutoMigrate(
		&User{}, &Session{}, &TestEnvironment{}, &SiteConfig{},
		&Commit{}, &TestRun{}, &TestArtifact{}, &Task{}, &TaskLog{},
	); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func isPostgres(dsn string) bool {
	return hasPrefix(dsn, "postgres://") || hasPrefix(dsn, "postgresql://")
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// --- User queries ---

// CreateUser inserts a new user record.
func (s *Store) CreateUser(u *User) error {
	return s.DB.Create(u).Error
}

// GetUserByUsername loads a user by username.
func (s *Store) GetUserByUsername(username string) (*User, error) {
	var u User
	if err := s.DB.Where("username = ?", username).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByID loads a user by id.
func (s *Store) GetUserByID(id int64) (*User, error) {
	var u User
	if err := s.DB.First(&u, id).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers returns every account, oldest first (the list the admin panel
// shows).
func (s *Store) ListUsers() ([]User, error) {
	var users []User
	if err := s.DB.Order("id").Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// UsernamesByID resolves user IDs to usernames in one query, so labelling a
// site-wide list (environments and their owners) does not become a query per
// row. IDs that match no account are absent from the result.
func (s *Store) UsernamesByID(ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var users []User
	if err := s.DB.Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, err
	}
	for i := range users {
		out[users[i].ID] = users[i].Username
	}
	return out, nil
}

// UpdateUser applies u to the account with the given id. It does not report
// whether the row existed — callers load the user first — and it never
// touches the role.
func (s *Store) UpdateUser(id int64, u UserUpdate) error {
	fields := map[string]any{
		"username": u.Username,
		"email":    u.Email,
		"disabled": u.Disabled,
	}
	if u.PasswordHash != "" {
		fields["password_hash"] = u.PasswordHash
	}
	return s.DB.Model(&User{}).Where("id = ?", id).Updates(fields).Error
}

// UsernameTaken reports whether another account (any id but exceptID) already
// uses the username.
func (s *Store) UsernameTaken(username string, exceptID int64) (bool, error) {
	return s.userFieldTaken("username", username, exceptID)
}

// EmailTaken reports whether another account (any id but exceptID) already
// uses the email address.
func (s *Store) EmailTaken(email string, exceptID int64) (bool, error) {
	return s.userFieldTaken("email", email, exceptID)
}

// userFieldTaken is the shared body of UsernameTaken and EmailTaken. field is
// one of the two literals above, never anything that came in over the wire.
func (s *Store) userFieldTaken(field, value string, exceptID int64) (bool, error) {
	var count int64
	err := s.DB.Model(&User{}).
		Where(field+" = ? AND id <> ?", value, exceptID).
		Count(&count).Error
	return count > 0, err
}

// --- Session queries ---

// CreateSession inserts a new session.
func (s *Store) CreateSession(sess *Session) error {
	return s.DB.Create(sess).Error
}

// GetSessionByToken loads a session and the associated user if the session has
// not expired.
func (s *Store) GetSessionByToken(token string) (*Session, *User, error) {
	var sess Session
	if err := s.DB.Where("token = ? AND expires_at > ?", token, time.Now()).First(&sess).Error; err != nil {
		return nil, nil, err
	}
	var u User
	if err := s.DB.First(&u, sess.UserID).Error; err != nil {
		return nil, nil, err
	}
	return &sess, &u, nil
}

// DeleteSession removes a session by token (no-op if it does not exist).
func (s *Store) DeleteSession(token string) error {
	return s.DB.Where("token = ?", token).Delete(&Session{}).Error
}

// DeleteSessionsForUser removes every session of the given user except the
// one named by exceptToken (pass "" to remove them all). Disabling an account
// ends every session; changing a password ends every session but the caller's
// own, so the person who made the change stays logged in.
func (s *Store) DeleteSessionsForUser(userID int64, exceptToken string) error {
	return s.DB.Where("user_id = ? AND token <> ?", userID, exceptToken).
		Delete(&Session{}).Error
}

// CleanExpiredSessions removes sessions whose expiry has passed. Returns the
// number of rows deleted.
func (s *Store) CleanExpiredSessions() (int64, error) {
	res := s.DB.Where("expires_at <= ?", time.Now()).Delete(&Session{})
	return res.RowsAffected, res.Error
}
