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

// BuildStageConfig is the build sub-task snapshot. Command is a CommandList
// (legacy snapshots storing a single string still decode); the commands run
// in order, stopping at the first failure, like the test stages.
type BuildStageConfig struct {
	Command     CommandList       `json:"command"`
	Description string            `json:"description,omitempty"` // human label stored with the build run
	Workdir     string            `json:"workdir,omitempty"`     // command workdir (empty = code dir)
	Artifacts   ArtifactPaths     `json:"artifacts,omitempty"`   // files fetched back after the build (stored as-is)
	Timeout     int               `json:"timeout"`               // seconds
	Env         map[string]string `json:"env,omitempty"`         // exported for the stage
}

// StageConfig is the unit sub-task snapshot: the command list with its
// workdir, timeout, environment and the optional artifact files the runner
// fetches back after the commands ran (a run can produce several; legacy
// snapshots store a single string and still decode).
type StageConfig struct {
	Command     CommandList       `json:"command"`
	Description string            `json:"description,omitempty"` // human label stored with the unit run
	Workdir     string            `json:"workdir,omitempty"`
	Timeout     int               `json:"timeout"` // seconds
	Env         map[string]string `json:"env,omitempty"`
	Artifacts   ArtifactPaths     `json:"artifacts,omitempty"` // paths relative to the workdir (or absolute)
}

// CaseStageConfig is one regression case sub-task snapshot: the preset name
// plus the resolved command list, workdir, timeout and artifact files.
type CaseStageConfig struct {
	Case        string            `json:"case"` // preset name
	Description string            `json:"description,omitempty"` // the case's (preset's) human label
	Command     CommandList       `json:"command"`
	Workdir     string            `json:"workdir,omitempty"`
	Timeout     int               `json:"timeout"`
	Env         map[string]string `json:"env,omitempty"`
	Artifacts   ArtifactPaths     `json:"artifacts,omitempty"`
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
