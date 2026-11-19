package store

import "time"

// PerformanceObservation is one series artifact of a performance run, with the
// context that places it on the development timeline: the commit it was
// measured on, when that commit arrived, and the machine that produced it.
//
// The performance module (server/performance) reads these, parses each
// artifact as a benchmark report and aggregates the measurements. The query
// lives here, with the rest of the SQL, so the module never has to know the
// table layout — and the layout can change without touching it.
type PerformanceObservation struct {
	Artifact  TestArtifact
	CommitID  int64
	CommitSHA string
	CommitAt  time.Time // commits.pushed_at: when the revision arrived at the site
	Repo      string
	Message   string
	EnvID     int64
	EnvName   string
}

// ListPerformanceObservations returns the series artifacts of performance runs
// pushed since the given time, oldest first, at most limit of them.
//
// A limit smaller than the number of matching artifacts keeps the NEWEST ones:
// the query walks back from the most recent commit, and the truncation is then
// turned around. A history page with a length setting wants exactly that — the
// recent past survives, not the oldest rows.
//
// envID 0 means every environment; repo "" means every repository.
func (s *Store) ListPerformanceObservations(since time.Time, envID int64, repo string, limit int) ([]PerformanceObservation, error) {
	// One flat row per artifact: the join's columns are named explicitly so a
	// future column on any of the four tables cannot leak into the scan (and
	// no password hash or private key is ever selected).
	rows := []struct {
		ArtifactID int64
		RunID      int64
		TaskID     int64
		Kind       string
		Name       string
		ObjectKey  string
		Size       int64
		Content    string
		CreatedAt  time.Time
		CommitID   int64
		CommitSHA  string
		CommitAt   time.Time
		Repo       string
		Message    string
		EnvID      int64
		EnvName    string
	}{}

	q := s.DB.Table("test_artifacts AS a").
		Select(`a.id AS artifact_id, a.run_id AS run_id, a.task_id AS task_id,
			a.kind AS kind, a.name AS name, a.object_key AS object_key,
			a.size AS size, a.content AS content, a.created_at AS created_at,
			c.id AS commit_id, c.sha AS commit_sha, c.pushed_at AS commit_at,
			c.repo AS repo, c.message AS message,
			e.id AS env_id, e.name AS env_name`).
		Joins("JOIN test_runs AS r ON r.id = a.run_id").
		Joins("JOIN commits AS c ON c.id = r.commit_id").
		Joins("JOIN test_environments AS e ON e.id = r.environment_id").
		// Only a performance stage's series artifacts are measurement reports;
		// a regression case may store series files too, and they are not.
		Where("a.kind = ? AND r.kind = ?", ArtifactKindSeries, RunKindPerformance).
		Where("c.pushed_at >= ?", since).
		Order("c.pushed_at DESC, a.id DESC").
		Limit(limit)
	if envID != 0 {
		q = q.Where("r.environment_id = ?", envID)
	}
	if repo != "" {
		q = q.Where("c.repo = ?", repo)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}

	// The query took the newest window; the caller gets it in chronological
	// order, which is how a chart reads it.
	out := make([]PerformanceObservation, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		out = append(out, PerformanceObservation{
			Artifact: TestArtifact{
				ID:        r.ArtifactID,
				RunID:     r.RunID,
				TaskID:    r.TaskID,
				Kind:      r.Kind,
				Name:      r.Name,
				ObjectKey: r.ObjectKey,
				Size:      r.Size,
				Content:   r.Content,
				CreatedAt: r.CreatedAt,
			},
			CommitID:  r.CommitID,
			CommitSHA: r.CommitSHA,
			CommitAt:  r.CommitAt,
			Repo:      r.Repo,
			Message:   r.Message,
			EnvID:     r.EnvID,
			EnvName:   r.EnvName,
		})
	}
	return out, nil
}
