package performance

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"md-builder/server/store"
)

const (
	// defaultDays is the history window a request that names none gets: half a
	// year is what a development timeline is usually read over.
	defaultDays = 180
	// maxDays is the longest window, about ten years — long enough to mean
	// "everything this site has", short enough to stay a number of days.
	maxDays = 3650
)

// GET /api/performance?days=180&environment=<id>
//
// The history of benchmark measurements: every performance run's reports in
// the window, parsed and aggregated per (case, metric, environment, commit).
// The page draws one scattershot per case from the response; it does no
// parsing of its own, so the report format lives in one place — this package.
func (s *Service) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	query := r.URL.Query()

	days, problem := parseDays(query.Get("days"))
	if problem != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": problem})
		return
	}
	envID, problem := parseEnvironment(query.Get("environment"))
	if problem != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": problem})
		return
	}

	hist, err := s.History(r.Context(), Options{
		Since: time.Now().AddDate(0, 0, -days),
		// A site that names its code repository shows only that repository's
		// history, exactly as the dashboard filters its columns.
		Repo:          s.siteRepo(),
		EnvironmentID: envID,
	})
	if err != nil {
		log.Printf("performance history: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, toHistoryJSON(hist, time.Now()))
}

// parseDays reads the days parameter, returning the reason it is unusable (and
// "" when it is fine).
func parseDays(raw string) (int, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultDays, ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxDays {
		return 0, fmt.Sprintf("days must be a whole number between 1 and %d", maxDays)
	}
	return n, ""
}

// parseEnvironment reads the environment parameter: an environment id, or 0
// for every environment when it is absent.
func parseEnvironment(raw string) (int64, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, ""
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, "environment must be an environment id"
	}
	return id, ""
}

// siteRepo is the repository this site tests, in the form commits.repo uses.
// Empty means the site names no code repository, and the history then covers
// every repository it has seen.
func (s *Service) siteRepo() string {
	cfg, err := s.Store.GetSiteConfig()
	if err != nil || cfg == nil {
		return ""
	}
	return store.RepoPath(cfg.CodeRepo)
}

// --- the response ---

// historyJSON is GET /api/performance. Points are flat, one per plotted
// measurement; the page groups them into traces by case, metric and
// environment. cases and environments say what the points contain, so a
// selector can offer exactly what there is to plot.
type historyJSON struct {
	Since        time.Time     `json:"since"`
	GeneratedAt  time.Time     `json:"generatedAt"`
	Truncated    bool          `json:"truncated"`
	Environments []envJSON     `json:"environments"`
	Cases        []caseJSON    `json:"cases"`
	Points       []pointJSON   `json:"points"`
	Ignored      []ignoredJSON `json:"ignored"`
}

type envJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type caseJSON struct {
	Name    string       `json:"name"`
	Metrics []metricJSON `json:"metrics"`
}

type metricJSON struct {
	Name   string `json:"name"`
	Unit   string `json:"unit"`
	Better string `json:"better"`
}

type pointJSON struct {
	Case          string    `json:"case"`
	Metric        string    `json:"metric"`
	Unit          string    `json:"unit"`
	Better        string    `json:"better"`
	EnvironmentID int64     `json:"environmentId"`
	Environment   string    `json:"environment"`
	At            time.Time `json:"at"`
	CommitID      int64     `json:"commitId"`
	SHA           string    `json:"sha"`
	Samples       int       `json:"samples"`
	Value         float64   `json:"value"`
}

type ignoredJSON struct {
	ArtifactID int64  `json:"artifactId"`
	RunID      int64  `json:"runId"`
	Name       string `json:"name"`
	Reason     string `json:"reason"`
}

func toHistoryJSON(h *History, generatedAt time.Time) historyJSON {
	out := historyJSON{
		Since:        h.Since,
		GeneratedAt:  generatedAt,
		Truncated:    h.Truncated,
		Environments: make([]envJSON, 0, len(h.Environments)),
		Cases:        make([]caseJSON, 0, len(h.Cases)),
		Points:       make([]pointJSON, 0, len(h.Points)),
		Ignored:      make([]ignoredJSON, 0, len(h.Ignored)),
	}
	for _, e := range h.Environments {
		out.Environments = append(out.Environments, envJSON{ID: e.ID, Name: e.Name})
	}
	for _, c := range h.Cases {
		metrics := make([]metricJSON, 0, len(c.Metrics))
		for _, m := range c.Metrics {
			metrics = append(metrics, metricJSON{Name: m.Name, Unit: m.Unit, Better: m.Better})
		}
		out.Cases = append(out.Cases, caseJSON{Name: c.Name, Metrics: metrics})
	}
	for _, p := range h.Points {
		out.Points = append(out.Points, pointJSON{
			Case:          p.Case,
			Metric:        p.Metric,
			Unit:          p.Unit,
			Better:        p.Better,
			EnvironmentID: p.EnvironmentID,
			Environment:   p.Environment,
			At:            p.At,
			CommitID:      p.CommitID,
			SHA:           p.SHA,
			Samples:       p.Samples,
			Value:         p.Value,
		})
	}
	for _, ig := range h.Ignored {
		out.Ignored = append(out.Ignored, ignoredJSON{
			ArtifactID: ig.ArtifactID,
			RunID:      ig.RunID,
			Name:       ig.Name,
			Reason:     ig.Reason,
		})
	}
	return out
}

// writeJSON is the module's own copy of the API's writer (deliberately: the
// module does not import the api package). Status first, then the body, as
// net/http expects.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
