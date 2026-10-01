package api

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"md-builder/server/store"

	"gorm.io/gorm"
)

// --- GET /api/tasks/{id}, /log and /log/download ---

// subTaskJSON is the wire representation of one sub-task.
type subTaskJSON struct {
	ID         int64   `json:"id"`
	Kind       string  `json:"kind"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Error      string  `json:"error"`
	DependsOn  []int64 `json:"dependsOn"`
	RunID      int64   `json:"runId,omitempty"` // test stages: the recorded run (for the detail link)
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
	Trigger       int               `json:"trigger"` // 0 = webhook, 1 = manual
	StartedAt     string            `json:"startedAt"`
	FinishedAt    string            `json:"finishedAt"`
	SubTasks      []subTaskJSON     `json:"subTasks,omitempty"` // roots only
	Commit        *commitJSON       `json:"commit,omitempty"`
	Environment   *dashboardEnvJSON `json:"environment,omitempty"`

	// RegressionRunID is the stage-wide regression run of a root: the graph
	// page's derived "reg test" node opens it, since each case node opens its
	// own case run instead.
	RegressionRunID int64 `json:"regressionRunId,omitempty"`
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
	if len(parts) == 2 && (parts[1] == "log" || parts[1] == "log/download") {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if parts[1] == "log" {
			s.taskLogs(w, r, id)
		} else {
			s.taskLogDownload(w, id)
		}
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
		Trigger:       task.Trigger,
	}
	if task.StartedAt != nil {
		detail.StartedAt = task.StartedAt.UTC().Format(timeFormat)
	}
	if task.FinishedAt != nil {
		detail.FinishedAt = task.FinishedAt.UTC().Format(timeFormat)
	}

	// Commit / environment context (null when the row was deleted).
	if commit, err := s.Store.GetCommitByID(task.CommitID); err == nil {
		cj := s.toCommitJSON(commit)
		detail.Commit = &cj
	}
	if env, err := s.Store.GetEnvironment(task.EnvironmentID); err == nil {
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
		// Test stages link to their recorded run (the stage detail view).
		var buildRuns, unitRuns, regRuns map[store.EnvCommit]store.TestRun
		if task.CommitID != 0 && task.EnvironmentID != 0 {
			ec := []int64{task.EnvironmentID}
			cc := []int64{task.CommitID}
			buildRuns, _ = s.Store.FindRunsByCommits(store.RunKindBuild, ec, cc)
			unitRuns, _ = s.Store.FindRunsByCommits(store.RunKindUnit, ec, cc)
			regRuns, _ = s.Store.FindRunsByCommits(store.RunKindRegression, ec, cc)
		}
		// A regression stage is one sub-task per case, and each case records
		// its own child run: resolve every case node to the run it produced,
		// so clicking a node opens that case rather than the whole stage.
		// (The parent regression run stays the fallback for a node whose case
		// never recorded one — an old dispatch, or a report without cases.)
		var caseRuns map[int64]store.TestRun
		if regIDs := regressionSubTaskIDs(subs); len(regIDs) > 0 {
			caseRuns, _ = s.Store.FindCaseRunsByTasks(regIDs)
		}
		detail.SubTasks = make([]subTaskJSON, 0, len(subs))
		for i := range subs {
			sj := toSubTaskJSON(&subs[i])
			key := store.EnvCommit{Env: subs[i].EnvironmentID, Commit: subs[i].CommitID}
			switch subs[i].Kind {
			case store.TaskKindBuild:
				if r, ok := buildRuns[key]; ok {
					sj.RunID = r.ID
				}
			case store.TaskKindUnit:
				if r, ok := unitRuns[key]; ok {
					sj.RunID = r.ID
				}
			case store.TaskKindRegression:
				if r, ok := caseRuns[subs[i].ID]; ok {
					sj.RunID = r.ID
				} else if r, ok := regRuns[key]; ok {
					sj.RunID = r.ID
				}
			}
			detail.SubTasks = append(detail.SubTasks, sj)
		}
		if r, ok := regRuns[store.EnvCommit{Env: task.EnvironmentID, Commit: task.CommitID}]; ok {
			detail.RegressionRunID = r.ID
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

// taskLogDownload handles GET /api/tasks/{id}/log/download: the task's whole
// log as one text file. The log viewer in the browser keeps only the tail of
// the stream it followed, so the file is produced from the stored chunks
// here, streamed batch by batch (ReadTaskLogs caps how many it returns).
func (s *Server) taskLogDownload(w http.ResponseWriter, id int64) {
	task, err := s.Store.GetTask(id)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		log.Printf("task log download: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("task-%d.log", task.ID)))
	// Walk the log by sequence: each batch continues after the last chunk
	// written, so a long build's output never has to fit in memory.
	after := 0
	for {
		logs, err := s.Store.ReadTaskLogs(task.ID, after)
		if err != nil {
			// The status line is already out: the download is cut short and
			// the reason stays in the server log.
			log.Printf("task %d log download: %v", task.ID, err)
			return
		}
		if len(logs) == 0 {
			return
		}
		for _, l := range logs {
			if _, err := io.WriteString(w, l.Content); err != nil {
				return // the client is gone; nothing left to write to
			}
			after = l.Seq
		}
	}
}

// regressionSubTaskIDs collects the ids of a root's regression case
// sub-tasks (each case is its own task and records its own child run).
func regressionSubTaskIDs(subs []store.Task) []int64 {
	var ids []int64
	for i := range subs {
		if subs[i].Kind == store.TaskKindRegression {
			ids = append(ids, subs[i].ID)
		}
	}
	return ids
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
