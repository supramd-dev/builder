package runner

import (
	"encoding/json"
	"fmt"
)

// Per-stage config snapshots stored on sub-task rows (JSON in Task.Config).
// They are self-contained: the executor needs no additional lookups to run
// a sub-task (only the environment row and site credentials come from
// elsewhere).

// CloneConfig is the (currently empty) config snapshot of the clone task.
// Kept as a type so future options (shallow vs full, include .git) have a
// place without a schema change.
type CloneConfig struct{}

// BuildStageConfig is the build sub-task snapshot.
type BuildStageConfig struct {
	Generator  string            `json:"generator"` // "cmake" | "script"
	CMakeFlags string            `json:"cmakeFlags,omitempty"`
	Threads    int               `json:"threads,omitempty"`
	Command    string            `json:"command,omitempty"` // generator: script
	Timeout    int               `json:"timeout"`           // seconds
	Env        map[string]string `json:"env,omitempty"`     // exported for the stage
}

// StageConfig is a test sub-task snapshot (unit / regression / future
// performance stages): one command with its timeout, the environment and the
// optional results files the runner fetches back after the command ran (a
// run can produce several; legacy snapshots store a single string and still
// decode).
type StageConfig struct {
	Command string            `json:"command"`
	Timeout int               `json:"timeout"` // seconds
	Env     map[string]string `json:"env,omitempty"`
	Results ResultsPaths      `json:"results,omitempty"` // paths relative to the code dir (or absolute)
}

// RootConfig is the root task's Config snapshot: the merged matrix entry.
type RootConfig struct {
	Entry MergedEntry `json:"entry"`
}

// marshalJSON is the shared JSON encoder for config snapshots.
func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal stage config: %w", err)
	}
	return string(b), nil
}
