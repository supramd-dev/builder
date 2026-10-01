package store

import (
	"time"
)

// TaskLog is one chunk of a task's output log. Logs are appended
// incrementally while a task runs (see runner.LogWriter) and read back
// incrementally by the API with afterSeq, so the frontend can follow live
// output with cheap polling.
//
// A log belongs to one attempt: a retried task keeps the previous attempt's
// output, and (task_id, attempt, seq) is unique — the writer seeds its
// sequence from the highest stored one, so a task resumed after a crash
// continues the same attempt instead of duplicating sequence numbers.
type TaskLog struct {
	ID        int64  `gorm:"primaryKey"`
	TaskID    int64  `gorm:"uniqueIndex:idx_task_logs_task_attempt_seq;not null"`
	Attempt   int    `gorm:"uniqueIndex:idx_task_logs_task_attempt_seq;not null"`
	Seq       int    `gorm:"uniqueIndex:idx_task_logs_task_attempt_seq;not null"` // per attempt, monotonic
	RunID     int64  `gorm:"not null;default:0"`                                  // the attempt's run (0 when unknown)
	Content   string `gorm:"type:text"`
	CreatedAt time.Time
}

// AppendTaskLog stores one log chunk. The (task_id, attempt, seq) triple is
// unique per write path (the LogWriter assigns sequential numbers), so no
// upsert is needed.
func (s *Store) AppendTaskLog(log *TaskLog) error {
	return s.DB.Create(log).Error
}

// logReadLimit caps one log read. A longer log is read page by page: the
// download handler streams batches, and the web viewer asks again while a
// full page comes back — so a reader that stops at one call would silently
// show the head of a long log and nothing else.
const logReadLimit = 1000

// ReadTaskLogs returns log chunks of one attempt with Seq > afterSeq, in
// order, at most logReadLimit of them: callers continue from the last Seq.
func (s *Store) ReadTaskLogs(taskID int64, attempt, afterSeq int) ([]TaskLog, error) {
	var logs []TaskLog
	q := s.DB.Where("task_id = ? AND attempt = ?", taskID, attempt)
	if afterSeq > 0 {
		q = q.Where("seq > ?", afterSeq)
	}
	if err := q.Order("seq ASC").Limit(logReadLimit).Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

// ReadTaskLogTail returns the end of one attempt's log, in order: the last
// logReadLimit chunks of it (the whole log when it is shorter). A reader that
// wants the stage's result — the summary line the runner derives from the
// output, the last errors of a failed command — must read from the end:
// ReadTaskLogs from zero hands out the head, so a long log would show an
// outcome taken from its first page.
func (s *Store) ReadTaskLogTail(taskID int64, attempt int) ([]TaskLog, error) {
	max, err := s.MaxTaskLogSeq(taskID, attempt)
	if err != nil || max == 0 {
		return nil, err
	}
	var logs []TaskLog
	// A short log has no earlier sequence to skip (the bound goes to a
	// non-positive number, which no seq is), so this is one query either way.
	if err := s.DB.Where("task_id = ? AND attempt = ? AND seq > ?", taskID, attempt, max-logReadLimit).
		Order("seq ASC").Limit(logReadLimit).Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

// MaxTaskLogSeq returns the highest stored log sequence of one attempt (0
// when it has no logs). The writer resumes from it, so a restarted attempt
// appends after its existing output.
func (s *Store) MaxTaskLogSeq(taskID int64, attempt int) (int, error) {
	var max int
	err := s.DB.Model(&TaskLog{}).Where("task_id = ? AND attempt = ?", taskID, attempt).
		Select("COALESCE(MAX(seq), 0)").Scan(&max).Error
	return max, err
}

// DeleteTaskLogs removes all log chunks of a task (every attempt).
func (s *Store) DeleteTaskLogs(taskID int64) error {
	return s.DB.Where("task_id = ?", taskID).Delete(&TaskLog{}).Error
}
