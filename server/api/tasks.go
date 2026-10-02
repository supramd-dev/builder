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

// --- GET /api/tasks/{id}, /log, /runs and /artifacts/zip ---

// subTaskJSON is the wire representation of one node of a task graph. A node is
// either a real task (it has attempts and runs) or a virtual container whose
// status and counts are its children's aggregate; retired nodes are the ones
// the current md-builder.yaml no longer defines, kept as history.
type subTaskJSON struct {
	ID          int64   `json:"id"`
	ParentID    int64   `json:"parentId,omitempty"` // the tree: where the node nests (0 = directly under the root)
	Kind        string  `json:"kind"`
	NodeKey     string  `json:"nodeKey"` // stable identity across dispatches
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Virtual     bool    `json:"virtual,omitempty"`
	Retired     bool    `json:"retired,omitempty"`
	Status      string  `json:"status"`
	Summary     string  `json:"summary,omitempty"`
	Error       string  `json:"error,omitempty"`
	DependsOn   []int64 `json:"dependsOn"`
	Total       int     `json:"total"`
	Passed      int     `json:"passed"`
	Failed      int     `json:"failed"`
	Skipped     int     `json:"skipped"`
	Attempts    int     `json:"attempts"`
	RunID       int64   `json:"runId,omitempty"` // the latest attempt's run (for the detail link)
	StartedAt   string  `json:"startedAt"`
	FinishedAt  string  `json:"finishedAt"`
}

// taskDetailJSON is GET /api/tasks/{id}'s response: the task (root or node)
// plus, for a root, its node list (the graph view). Every node's own task
// detail carries the attempts of the test it stands for.
type taskDetailJSON struct {
	ID            int64             `json:"id"`
	RootID        int64             `json:"rootId"`
	ParentID      int64             `json:"parentId,omitempty"`
	Kind          string            `json:"kind"`
	NodeKey       string            `json:"nodeKey"`
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	Virtual       bool              `json:"virtual,omitempty"`
	Retired       bool              `json:"retired,omitempty"`
	Status        string            `json:"status"`
	Summary       string            `json:"summary,omitempty"`
	Error         string            `json:"error,omitempty"`
	Total         int               `json:"total"`
	Passed        int               `json:"passed"`
	Failed        int               `json:"failed"`
	Skipped       int               `json:"skipped"`
	Attempts      int               `json:"attempts"` // dispatches: for a real task, its attempt count
	CommitID      int64             `json:"commitId"`
	EnvironmentID int64             `json:"environmentId"`
	Tags          string            `json:"tags"`
	Trigger       int               `json:"trigger"` // 0 = webhook, 1 = manual, 2 = manual-yaml
	StartedAt     string            `json:"startedAt"`
	FinishedAt    string            `json:"finishedAt"`
	Runs          []runJSON         `json:"runs,omitempty"`         // real tasks: every attempt, newest first
	SubTasks      []subTaskJSON     `json:"subTasks,omitempty"`     // roots: the current nodes
	RetiredTasks  []subTaskJSON     `json:"retiredTasks,omitempty"` // roots: nodes a later dispatch dropped
	Commit        *commitJSON       `json:"commit,omitempty"`
	Environment   *dashboardEnvJSON `json:"environment,omitempty"`
}

// handleTaskItem routes /api/tasks/{id} and its sub-resources: /log,
// /log/download, /runs and /artifacts/zip.
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
	if len(parts) == 2 {
		if parts[1] == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		switch parts[1] {
		case "log":
			s.taskLogs(w, r, id)
		case "log/download":
			s.taskLogDownload(w, r, id)
		case "runs":
			s.taskRuns(w, id)
		case "artifacts/zip":
			s.downloadTaskArtifactsZip(w, r, id)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		}
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
	task, ok := s.loadTask(w, id, "task detail")
	if !ok {
		return
	}

	detail := taskDetailJSON{
		ID:            task.ID,
		RootID:        task.RootID,
		ParentID:      task.ParentID,
		Kind:          task.Kind,
		NodeKey:       task.NodeKey,
		Name:          task.Name,
		Description:   task.Description,
		Virtual:       task.Virtual,
		Retired:       task.Retired,
		Status:        task.Status,
		Summary:       task.Summary,
		Error:         task.Error,
		Total:         task.Total,
		Passed:        task.Passed,
		Failed:        task.Failed,
		Skipped:       task.Skipped,
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

	// A real task's attempts: the run page of each one is where its log and
	// artifacts live, so the task detail lists them.
	if !task.Virtual {
		runs, err := s.Store.ListTaskRuns(task.ID)
		if err != nil {
			log.Printf("task detail runs: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		detail.Runs = make([]runJSON, 0, len(runs))
		for i := range runs {
			detail.Runs = append(detail.Runs, toRunJSON(&runs[i]))
		}
	}

	// For a root, attach the node list: the current graph plus the nodes an
	// earlier dispatch defined and a later one dropped.
	if task.Kind == store.TaskKindRoot {
		active, err := s.Store.ListActiveNodes(task.ID)
		if err != nil {
			log.Printf("task detail nodes: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		retired, err := s.Store.ListRetiredNodes(task.ID)
		if err != nil {
			log.Printf("task detail retired nodes: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		// Each node links to the run of its latest attempt (the node's log and
		// artifacts); a virtual node has none.
		runs, err := s.Store.LatestRunsByTaskIDs(taskIDsOf(active, retired))
		if err != nil {
			log.Printf("task detail node runs: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		detail.SubTasks = make([]subTaskJSON, 0, len(active))
		for i := range active {
			detail.SubTasks = append(detail.SubTasks, toSubTaskJSON(&active[i], runOf(runs, active[i].ID)))
		}
		detail.RetiredTasks = make([]subTaskJSON, 0, len(retired))
		for i := range retired {
			detail.RetiredTasks = append(detail.RetiredTasks, toSubTaskJSON(&retired[i], runOf(runs, retired[i].ID)))
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

// taskRuns handles GET /api/tasks/{id}/runs: the task's attempts, newest
// first (one run per attempt), for the log viewer's attempt switcher.
func (s *Server) taskRuns(w http.ResponseWriter, id int64) {
	task, ok := s.loadTask(w, id, "task runs")
	if !ok {
		return
	}
	if task.Virtual {
		writeJSON(w, http.StatusOK, map[string]any{"runs": []runJSON{}})
		return
	}
	runs, err := s.Store.ListTaskRuns(task.ID)
	if err != nil {
		log.Printf("task runs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]runJSON, 0, len(runs))
	for i := range runs {
		out = append(out, toRunJSON(&runs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// taskLogs handles GET /api/tasks/{id}/log?after=<seq>&attempt=<n>: log chunks
// after the given sequence, for incremental (live-following) reads. Without an
// attempt the current one is read — what a viewer following a task live wants.
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
	task, ok := s.loadTask(w, id, "task logs")
	if !ok {
		return
	}
	attempt, ok := s.readAttemptParam(w, r, task)
	if !ok {
		return
	}
	logs, err := s.Store.ReadTaskLogs(id, attempt, after)
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
		"attempt": attempt,
		"chunks":  chunks,
		"lastSeq": lastSeq,
	})
}

// logChunkJSON is one stored log chunk.
type logChunkJSON struct {
	Seq     int    `json:"seq"`
	Content string `json:"content"`
}

// taskLogDownload handles GET /api/tasks/{id}/log/download[?attempt=<n>]: the
// attempt's whole log as one text file. The log viewer in the browser keeps
// only the tail of the stream it followed, so the file is produced from the
// stored chunks here, streamed batch by batch (ReadTaskLogs caps how many it
// returns).
func (s *Server) taskLogDownload(w http.ResponseWriter, r *http.Request, id int64) {
	task, ok := s.loadTask(w, id, "task log download")
	if !ok {
		return
	}
	attempt, ok := s.readAttemptParam(w, r, task)
	if !ok {
		return
	}
	name := fmt.Sprintf("task-%d", task.ID)
	if attempt != task.Attempts {
		name = fmt.Sprintf("%s-attempt-%d", name, attempt)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".log"))
	// Walk the log by sequence: each batch continues after the last chunk
	// written, so a long build's output never has to fit in memory.
	after := 0
	for {
		logs, err := s.Store.ReadTaskLogs(task.ID, attempt, after)
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

// loadTask loads a task by id and answers the request itself on failure
// (404/500); ok reports whether the caller should carry on.
func (s *Server) loadTask(w http.ResponseWriter, id int64, what string) (*store.Task, bool) {
	task, err := s.Store.GetTask(id)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return nil, false
		}
		log.Printf("%s: %v", what, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	return task, true
}

// readAttemptParam resolves an attempt number off the query string, defaulting
// to the task's current attempt (what a live viewer wants). An unknown attempt
// is not an error: an attempt that never logged anything reads as empty, and
// the caller can tell which one it asked for from the response.
func (s *Server) readAttemptParam(w http.ResponseWriter, r *http.Request, task *store.Task) (int, bool) {
	v := r.URL.Query().Get("attempt")
	if v == "" {
		return task.Attempts, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "attempt must be a positive integer"})
		return 0, false
	}
	return n, true
}

// taskIDsOf collects the ids of one or more node lists.
func taskIDsOf(lists ...[]store.Task) []int64 {
	var ids []int64
	for _, list := range lists {
		for i := range list {
			ids = append(ids, list[i].ID)
		}
	}
	return ids
}

func toSubTaskJSON(t *store.Task, run *store.TestRun) subTaskJSON {
	out := subTaskJSON{
		ID:          t.ID,
		ParentID:    t.ParentID,
		Kind:        t.Kind,
		NodeKey:     t.NodeKey,
		Name:        t.Name,
		Description: t.Description,
		Virtual:     t.Virtual,
		Retired:     t.Retired,
		Status:      t.Status,
		Summary:     t.Summary,
		Error:       t.Error,
		DependsOn:   t.DependsOnIDs(),
		Total:       t.Total,
		Passed:      t.Passed,
		Failed:      t.Failed,
		Skipped:     t.Skipped,
		Attempts:    t.Attempts,
	}
	if out.DependsOn == nil {
		out.DependsOn = []int64{}
	}
	if run != nil {
		out.RunID = run.ID
	}
	if t.StartedAt != nil {
		out.StartedAt = t.StartedAt.UTC().Format(timeFormat)
	}
	if t.FinishedAt != nil {
		out.FinishedAt = t.FinishedAt.UTC().Format(timeFormat)
	}
	return out
}
