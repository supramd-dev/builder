package store

import (
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Commit event kinds: what created the row. Webhook events carry GitLab's
// object_kind; manual triggers record their own kind. The event lives on the
// commit (not the task): it is a property of the code revision entering the
// matrix, while the task's Trigger column records the dispatch mechanism
// (webhook / manual / manual-yaml). Deduplicated rows keep the event of the
// FIRST recording (a tag push of an already-pushed SHA requeues the existing
// row instead of duplicating it).
const (
	CommitEventPush         = "push"
	CommitEventTagPush      = "tag_push"
	CommitEventMergeRequest = "merge_request"
	CommitEventManual       = "manual"
	CommitEventManualYAML   = "manual_yaml"
)

// Commit records a git push received via the GitLab webhook — the code
// revision a column of the test dashboard corresponds to. (repo, sha) is
// NOT unique: manual triggers record one row per dispatch of the same SHA
// (each gets its own matrix row); webhook pushes still deduplicate through
// GetOrCreateCommit.
type Commit struct {
	ID        int64     `gorm:"primaryKey"`
	Repo      string    `gorm:"index:idx_commits_repo_sha;not null"` // project path, e.g. "group/md-code"
	SHA       string    `gorm:"index:idx_commits_repo_sha;not null"` // the pushed commit id
	Ref       string    `gorm:"not null;default:''"`                 // branch name, e.g. "main"
	Author    string    `gorm:"not null;default:''"`                 // the pushing user
	Message   string    `gorm:"not null;default:''"`                // head commit title
	Event     string    `gorm:"not null;default:''"`                // what created the row: push | tag_push | merge_request | manual | manual_yaml (CommitEvent*)
	PushedAt  time.Time `gorm:"not null"`                            // when the push was received
	CreatedAt time.Time
}

// --- Commit queries ---

// CreateCommit inserts a commit record.
func (s *Store) CreateCommit(c *Commit) error {
	return s.DB.Create(c).Error
}

// GetOrCreateCommit returns the commit for (repo, sha), inserting a new row
// when none exists yet. create reports whether a new row was inserted.
// Re-recording an existing row (a tag push of a pushed SHA, a merge of a
// pushed branch, a manual-yaml dispatch) restamps the event — and ref/message
// when the new recording provides them — so the row reflects the event that
// currently tests it; callers passing an empty event (a plain re-dispatch)
// leave the stored one alone.
func (s *Store) GetOrCreateCommit(c *Commit) (created bool, err error) {
	incoming := *c
	err = s.DB.Where("repo = ? AND sha = ?", c.Repo, c.SHA).First(c).Error
	if err == nil {
		restampCommit(s.DB, c, incoming)
		return false, nil
	}
	if err != ErrNotFound {
		return false, err
	}
	if err := s.DB.Create(c).Error; err != nil {
		// A concurrent push may have created it first; load then.
		if lerr := s.DB.Where("repo = ? AND sha = ?", c.Repo, c.SHA).First(c).Error; lerr == nil {
			restampCommit(s.DB, c, incoming)
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// restampCommit updates the event (and ref/message) of an existing commit row
// from a newer recording of the same SHA, in place. Empty fields in the
// incoming recording never overwrite stored values.
func restampCommit(db *gorm.DB, stored *Commit, incoming Commit) {
	updates := map[string]any{}
	if incoming.Event != "" && incoming.Event != stored.Event {
		updates["event"] = incoming.Event
	}
	if incoming.Ref != "" && incoming.Ref != stored.Ref {
		updates["ref"] = incoming.Ref
	}
	if incoming.Message != "" && incoming.Message != stored.Message {
		updates["message"] = incoming.Message
	}
	if len(updates) == 0 {
		return
	}
	if err := db.Model(stored).Updates(updates).Error; err == nil {
		if v, ok := updates["event"]; ok {
			stored.Event, _ = v.(string)
		}
		if v, ok := updates["ref"]; ok {
			stored.Ref, _ = v.(string)
		}
		if v, ok := updates["message"]; ok {
			stored.Message, _ = v.(string)
		}
	}
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
