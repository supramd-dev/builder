package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"md-builder/server/store"

	"gorm.io/gorm"
)

// --- GET /api/tasks/{id} and GET /api/tasks/{id}/log ---

// subTaskJSON is the wire representation of one sub-task.
type subTaskJSON struct {
	ID         int64   `json:"id"`
	Kind       string  `json:"kind"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Error      string  `json:"error"`
	DependsOn  []int64 `json:"dependsOn"`
	StartedAt  string  `json:"startedAt"`
	FinishedAt string  `json:"finishedAt"`
}

// taskDetailJSON is GET /api/tasks/{id}'s response: the task (root or
// sub-task) plus, for a root, its sub-task list (the graph view).
type taskDetailJSON struct {
	ID            int64             `json:"id"`
	RootID        int64             `json:"rootId"`
	Kind          string            `json:"kind"`
	Name          string            `json:"name"`
	Status        string            `json:"status"`
	Error         string            `json:"error"`
	Attempts      int               `json:"attempts"`
	CommitID      int64             `json:"commitId"`
	EnvironmentID int64             `json:"environmentId"`
	Tags          string            `json:"tags"`
	StartedAt     string            `json:"startedAt"`
	FinishedAt    string            `json:"finishedAt"`
	SubTasks      []subTaskJSON     `json:"subTasks,omitempty"` // roots only
	Commit        *commitJSON       `json:"commit,omitempty"`
	Environment   *dashboardEnvJSON `json:"environment,omitempty"`
}

// handleTaskItem routes /api/tasks/{id} (and /log).
func (s *Server) handleTaskItem(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	rest := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	if rest == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid task id"})
		return
	}
	if len(parts) == 2 && parts[1] == "log" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.taskLogs(w, r, id)
		return
	}
	if len(parts) != 1 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.taskDetail(w, id)
}

// taskDetail handles GET /api/tasks/{id}.
func (s *Server) taskDetail(w http.ResponseWriter, id int64) {
	task, err := s.Store.GetTask(id)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		log.Printf("task detail: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	detail := taskDetailJSON{
		ID:            task.ID,
		RootID:        task.RootID,
		Kind:          task.Kind,
		Name:          task.Name,
		Status:        task.Status,
		Error:         task.Error,
		Attempts:      task.Attempts,
		CommitID:      task.CommitID,
		EnvironmentID: task.EnvironmentID,
		Tags:          task.Tags,
	}
	if task.StartedAt != nil {
		detail.StartedAt = task.StartedAt.UTC().Format(timeFormat)
	}
	if task.FinishedAt != nil {
		detail.FinishedAt = task.FinishedAt.UTC().Format(timeFormat)
	}

	// Commit / environment context (null when the row was deleted).
	if commit, err := s.Store.GetCommitByID(task.CommitID); err == nil {
		cj := toCommitJSON(commit)
		detail.Commit = &cj
	}
	if env, err := s.Store.GetEnvironmentAny(task.EnvironmentID); err == nil {
		ev := dashboardEnvJSON{
			ID:          env.ID,
			Name:        env.Name,
			Description: env.Description,
			Tags:        env.Tags,
			Enabled:     env.Enabled,
		}
		detail.Environment = &ev
	}

	// For a root, attach the sub-task list.
	if task.Kind == store.TaskKindRoot {
		subs, err := s.Store.ListSubTasks(task.ID)
		if err != nil {
			log.Printf("task detail subs: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		detail.SubTasks = make([]subTaskJSON, 0, len(subs))
		for i := range subs {
			detail.SubTasks = append(detail.SubTasks, toSubTaskJSON(&subs[i]))
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

// taskLogs handles GET /api/tasks/{id}/log?after=<seq>: log chunks after
// the given sequence, for incremental (live-following) reads.
func (s *Server) taskLogs(w http.ResponseWriter, r *http.Request, id int64) {
	after := 0
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "after must be a non-negative integer"})
			return
		}
		after = n
	}
	if _, err := s.Store.GetTask(id); err != nil {
		if errors.Is(err, store.ErrTaskNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		log.Printf("task logs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	logs, err := s.Store.ReadTaskLogs(id, after)
	if err != nil {
		log.Printf("task logs read: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	chunks := make([]logChunkJSON, 0, len(logs))
	lastSeq := after
	for _, l := range logs {
		chunks = append(chunks, logChunkJSON{Seq: l.Seq, Content: l.Content})
		if l.Seq > lastSeq {
			lastSeq = l.Seq
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chunks":  chunks,
		"lastSeq": lastSeq,
	})
}

// logChunkJSON is one stored log chunk.
type logChunkJSON struct {
	Seq     int    `json:"seq"`
	Content string `json:"content"`
}

func toSubTaskJSON(t *store.Task) subTaskJSON {
	out := subTaskJSON{
		ID:        t.ID,
		Kind:      t.Kind,
		Name:      t.Name,
		Status:    t.Status,
		Error:     t.Error,
		DependsOn: t.DependsOnIDs(),
	}
	if out.DependsOn == nil {
		out.DependsOn = []int64{}
	}
	if t.StartedAt != nil {
		out.StartedAt = t.StartedAt.UTC().Format(timeFormat)
	}
	if t.FinishedAt != nil {
		out.FinishedAt = t.FinishedAt.UTC().Format(timeFormat)
	}
	return out
}
