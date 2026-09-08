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
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// User is an application user. PasswordHash holds a bcrypt hash, never a
// plaintext password.
type User struct {
	ID           int64  `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;not null"`
	Email        string `gorm:"uniqueIndex;not null"`
	PasswordHash string `gorm:"not null"`
	CreatedAt    time.Time
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

// Store wraps a *gorm.DB.
type Store struct {
	DB *gorm.DB
}

// Open opens the database described by dsn and runs AutoMigrate.
func Open(dsn string) (*Store, error) {
	db, err := openDB(dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db}
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
		db, err = gorm.Open(sqlite.Open(dsn), gormCfg)
	}
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return db, nil
}

// migrate creates/updates the schema via GORM AutoMigrate.
func (s *Store) migrate() error {
	if err := s.DB.AutoMigrate(&User{}, &Session{}, &TestEnvironment{}); err != nil {
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

// CleanExpiredSessions removes sessions whose expiry has passed. Returns the
// number of rows deleted.
func (s *Store) CleanExpiredSessions() (int64, error) {
	res := s.DB.Where("expires_at <= ?", time.Now()).Delete(&Session{})
	return res.RowsAffected, res.Error
}
