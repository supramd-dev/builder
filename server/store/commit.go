package store

import (
	"net/url"
	"strings"
	"time"
)

// Commit records a git push received via the GitLab webhook — the code
// revision a column of the test dashboard corresponds to.
type Commit struct {
	ID        int64     `gorm:"primaryKey"`
	Repo      string    `gorm:"uniqueIndex:idx_commits_repo_sha;not null"` // project path, e.g. "group/md-code"
	SHA       string    `gorm:"uniqueIndex:idx_commits_repo_sha;not null"` // the pushed commit id
	Ref       string    `gorm:"not null;default:''"`                       // branch name, e.g. "main"
	Author    string    `gorm:"not null;default:''"`                       // the pushing user
	Message   string    `gorm:"not null;default:''"`                       // head commit title
	PushedAt  time.Time `gorm:"not null"`                                  // when the push was received
	CreatedAt time.Time
}

// --- Commit queries ---

// CreateCommit inserts a commit record.
func (s *Store) CreateCommit(c *Commit) error {
	return s.DB.Create(c).Error
}

// GetOrCreateCommit returns the commit for (repo, sha), inserting a new row
// when none exists yet. create reports whether a new row was inserted.
func (s *Store) GetOrCreateCommit(c *Commit) (created bool, err error) {
	err = s.DB.Where("repo = ? AND sha = ?", c.Repo, c.SHA).First(c).Error
	if err == nil {
		return false, nil
	}
	if err != ErrNotFound {
		return false, err
	}
	if err := s.DB.Create(c).Error; err != nil {
		// A concurrent push may have created it first; load then.
		if lerr := s.DB.Where("repo = ? AND sha = ?", c.Repo, c.SHA).First(c).Error; lerr == nil {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GetCommitByID loads a commit by its internal id.
func (s *Store) GetCommitByID(id int64) (*Commit, error) {
	var c Commit
	if err := s.DB.First(&c, id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// ListCommits returns up to limit most-recent commits for repo (or for all
// repositories when repo is empty), newest first.
func (s *Store) ListCommits(repo string, limit int) ([]Commit, error) {
	q := s.DB
	if repo != "" {
		q = q.Where("repo = ?", repo)
	}
	var commits []Commit
	if err := q.Order("pushed_at DESC, id DESC").Limit(limit).Find(&commits).Error; err != nil {
		return nil, err
	}
	return commits, nil
}

// RepoPath extracts the "group/project" path from a repository location as
// used in the site config — an https URL, an SSH remote, or a bare
// "host/group/project" string. It is used to match webhook payloads (which
// carry path_with_namespace) against the configured code repository.
func RepoPath(location string) string {
	loc := strings.TrimSpace(location)
	if loc == "" {
		return ""
	}

	var path string
	switch {
	case strings.Contains(loc, "://"):
		// http(s)://host/group/code or ssh://user@host:port/group/code.
		if u, err := url.Parse(loc); err == nil {
			path = u.Path
		}
	case strings.Contains(loc, "@") && strings.Contains(loc, ":"):
		// scp-style remote: git@host:group/code.
		if i := strings.LastIndexByte(loc, ':'); i >= 0 {
			path = loc[i+1:]
		}
	default:
		// Bare "host/group/code" or "host:group/code": drop the host segment.
		if i := strings.IndexAny(loc, "/:"); i >= 0 {
			if loc[i] == ':' && !strings.Contains(loc[:i], "/") {
				// host:group/code without user@.
				path = loc[i+1:]
			} else {
				path = loc[i+1:]
			}
		}
	}
	return strings.Trim(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
}
