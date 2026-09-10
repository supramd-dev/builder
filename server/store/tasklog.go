package store

import (
	"time"
)

// TaskLog is one chunk of a task's output log. Logs are appended
// incrementally while the task runs (see runner.LogWriter) and read back
// incrementally by the API with afterSeq, so the frontend can follow live
// output with cheap polling.
type TaskLog struct {
	ID        int64  `gorm:"primaryKey"`
	TaskID    int64  `gorm:"index:idx_task_logs_task_seq;not null"`
	Seq       int    `gorm:"index:idx_task_logs_task_seq;not null"` // per-task monotonic
	Content   string `gorm:"type:text"`
	CreatedAt time.Time
}

// AppendTaskLog stores one log chunk. The (task_id, seq) pair is unique per
// write path (the LogWriter assigns sequential numbers), so no upsert is
// needed.
func (s *Store) AppendTaskLog(taskID int64, seq int, content string) error {
	return s.DB.Create(&TaskLog{TaskID: taskID, Seq: seq, Content: content}).Error
}

// ReadTaskLogs returns log chunks of a task with Seq > afterSeq, in order.
func (s *Store) ReadTaskLogs(taskID int64, afterSeq int) ([]TaskLog, error) {
	var logs []TaskLog
	q := s.DB.Where("task_id = ?", taskID)
	if afterSeq > 0 {
		q = q.Where("seq > ?", afterSeq)
	}
	if err := q.Order("seq ASC").Limit(1000).Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

// MaxTaskLogSeq returns the highest stored log sequence of a task (0 when it
// has no logs).
func (s *Store) MaxTaskLogSeq(taskID int64) (int, error) {
	var max int
	err := s.DB.Model(&TaskLog{}).Where("task_id = ?", taskID).
		Select("COALESCE(MAX(seq), 0)").Scan(&max).Error
	return max, err
}

// DeleteTaskLogs removes all log chunks of a task.
func (s *Store) DeleteTaskLogs(taskID int64) error {
	return s.DB.Where("task_id = ?", taskID).Delete(&TaskLog{}).Error
}
