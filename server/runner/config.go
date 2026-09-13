// Package runner implements the single-job execution pipeline: parse the
// md-builder.yaml test matrix from the code repository, generate the remote
// bash script, execute it over SSH and turn the line-protocol report into
// database rows.
package runner

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"md-builder/server/store"
)

// ConfigVersion is the only supported md-builder.yaml schema version.
const ConfigVersion = 1

// MaxTimeoutSeconds is the hard per-command timeout cap (4h).
const MaxTimeoutSeconds = 4 * 3600

// DefaultTimeoutSeconds applies when neither defaults nor the entry set one.
const DefaultTimeoutSeconds = 3600

// DefaultBuildThreads is the default parallel build count.
const DefaultBuildThreads = 8

// Build generators.
const (
	GeneratorCMake  = "cmake"
	GeneratorScript = "script"
)

// EnvConfig is a test stage: unit or regression.
type EnvConfig struct {
	Command string       `yaml:"command" json:"command"`
	Timeout int          `yaml:"timeout,omitempty" json:"timeout,omitempty"` // seconds; 0 = use default
	Results ResultsPaths `yaml:"results,omitempty" json:"results,omitempty"` // googletest results files (XML/JSON) produced by the command
}

// ResultsPaths is a list of results file paths. A run (or a single case) can
// produce several of them, so the yaml/JSON form accepts either one scalar
// path or a list:
//
//	results: "build/test_detail.xml"
//	results: ["build/test_detail.xml", "build/extra.json"]
type ResultsPaths []string

// UnmarshalYAML accepts a scalar path or a list of paths.
func (r *ResultsPaths) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Value == "" && node.Tag == "!!null" {
			*r = nil
			return nil
		}
		*r = ResultsPaths{node.Value}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := node.Decode(&list); err != nil {
			return err
		}
		*r = list
		return nil
	default:
		return fmt.Errorf("results must be a path or a list of paths")
	}
}

// UnmarshalJSON accepts a scalar path or a list of paths (legacy snapshots
// and API bodies stored a single string).
func (r *ResultsPaths) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" || s == "" {
		*r = nil
		return nil
	}
	if s[0] == '[' {
		var list []string
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*r = list
		return nil
	}
	var one string
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	*r = ResultsPaths{one}
	return nil
}

// MarshalJSON emits the list form (one file → a one-element list keeps the
// shape stable).
func (r ResultsPaths) MarshalJSON() ([]byte, error) {
	list := []string(r)
	if list == nil {
		list = []string{}
	}
	return json.Marshal(list)
}

// Clean returns the paths trimmed, deduplicated, with empties dropped.
func (r ResultsPaths) Clean() ResultsPaths {
	seen := map[string]bool{}
	out := make(ResultsPaths, 0, len(r))
	for _, p := range r {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// BuildConfig describes how to compile the code repository.
type BuildConfig struct {
	Generator  string `yaml:"generator,omitempty" json:"generator,omitempty"` // cmake (default) | script
	CMakeFlags string `yaml:"cmake_flags,omitempty" json:"cmake_flags,omitempty"`
	Threads    int    `yaml:"threads,omitempty" json:"threads,omitempty"`
	Command    string `yaml:"command,omitempty" json:"command,omitempty"` // generator: script
}

// EntryConfig is one matrix entry, before defaults are merged in.
type EntryConfig struct {
	Tags        []string          `yaml:"tags" json:"tags"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Build       BuildConfig       `yaml:"build,omitempty" json:"build,omitempty"`
	Unit        *EnvConfig        `yaml:"unit,omitempty" json:"unit,omitempty"`
	Regression  *EnvConfig        `yaml:"regression,omitempty" json:"regression,omitempty"`
	Timeout     int               `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// rawConfig mirrors md-builder.yaml before defaults merging.
type rawConfig struct {
	Version  int           `yaml:"version"`
	Defaults *EntryConfig  `yaml:"defaults,omitempty"`
	Matrix   []EntryConfig `yaml:"matrix"`
}

// MergedEntry is an entry with defaults applied — the config snapshot stored
// on a job and used to generate the remote script.
type MergedEntry struct {
	Tags        []string          `json:"tags"`
	Description string            `json:"description,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Build       BuildConfig       `json:"build"`
	Unit        *EnvConfig        `json:"unit,omitempty"`
	Regression  *EnvConfig        `json:"regression,omitempty"`
	Timeout     int               `json:"timeout"`
}

// ParseConfig parses and validates md-builder.yaml. The returned entries are
// defaults-merged and ready for environment matching.
func ParseConfig(data []byte) ([]MergedEntry, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse md-builder.yaml: %w", err)
	}
	if raw.Version != ConfigVersion {
		return nil, fmt.Errorf("md-builder.yaml: version must be %d, got %d", ConfigVersion, raw.Version)
	}
	if len(raw.Matrix) == 0 {
		return nil, fmt.Errorf("md-builder.yaml: matrix must not be empty")
	}

	seen := map[string]bool{}
	out := make([]MergedEntry, 0, len(raw.Matrix))
	for i := range raw.Matrix {
		entry := raw.Matrix[i]
		if len(entry.Tags) == 0 {
			return nil, fmt.Errorf("md-builder.yaml: matrix entry %d has no tags", i+1)
		}
		if entry.Unit == nil && entry.Regression == nil {
			return nil, fmt.Errorf("md-builder.yaml: matrix entry %d defines neither unit nor regression", i+1)
		}
		if entry.Unit != nil && strings.TrimSpace(entry.Unit.Command) == "" {
			return nil, fmt.Errorf("md-builder.yaml: matrix entry %d has unit without a command", i+1)
		}
		if entry.Regression != nil && strings.TrimSpace(entry.Regression.Command) == "" {
			return nil, fmt.Errorf("md-builder.yaml: matrix entry %d has regression without a command", i+1)
		}
		if entry.Build.Generator != "" && entry.Build.Generator != GeneratorCMake && entry.Build.Generator != GeneratorScript {
			return nil, fmt.Errorf("md-builder.yaml: matrix entry %d: build.generator must be cmake or script", i+1)
		}
		if entry.Build.Generator == GeneratorScript && strings.TrimSpace(entry.Build.Command) == "" {
			return nil, fmt.Errorf("md-builder.yaml: matrix entry %d: build.generator script requires build.command", i+1)
		}

		tags := normalizeTagList(entry.Tags)
		key := strings.Join(tags, ",")
		if seen[key] {
			return nil, fmt.Errorf("md-builder.yaml: duplicate matrix entry tags [%s]", key)
		}
		seen[key] = true

		out = append(out, mergeDefaults(raw.Defaults, &entry, tags))
	}
	return out, nil
}

// mergeDefaults applies defaults onto an entry: env maps are merged key-wise
// (entry wins), scalars are taken from the entry when set.
func mergeDefaults(defaults, entry *EntryConfig, tags []string) MergedEntry {
	m := MergedEntry{
		Tags:        tags,
		Description: entry.Description,
		Unit:        entry.Unit,
		Regression:  entry.Regression,
	}
	if m.Unit != nil {
		c := *m.Unit
		m.Unit = &c
	}
	if m.Regression != nil {
		c := *m.Regression
		m.Regression = &c
	}
	if m.Unit != nil && m.Unit.Timeout == 0 {
		m.Unit.Timeout = defaultTimeoutFor(defaults, entry)
	}
	if m.Regression != nil && m.Regression.Timeout == 0 {
		m.Regression.Timeout = defaultTimeoutFor(defaults, entry)
	}

	m.Env = map[string]string{}
	if defaults != nil {
		for k, v := range defaults.Env {
			m.Env[k] = v
		}
	}
	for k, v := range entry.Env {
		m.Env[k] = v
	}
	if len(m.Env) == 0 {
		m.Env = nil
	}

	m.Timeout = defaultTimeoutFor(defaults, entry)

	build := entry.Build
	if defaults != nil {
		if build.Generator == "" {
			build.Generator = defaults.Build.Generator
		}
		if build.CMakeFlags == "" {
			build.CMakeFlags = defaults.Build.CMakeFlags
		}
		if build.Command == "" {
			build.Command = defaults.Build.Command
		}
		if build.Threads == 0 {
			build.Threads = defaults.Build.Threads
		}
	}
	if build.Generator == "" {
		build.Generator = GeneratorCMake
	}
	if build.Threads == 0 {
		build.Threads = DefaultBuildThreads
	}
	m.Build = build
	return m
}

// defaultTimeoutFor resolves the per-command timeout (seconds): entry's own
// value wins, else defaults.Timeout, else the package default. It caps at the
// hard maximum.
func defaultTimeoutFor(defaults *EntryConfig, entry *EntryConfig) int {
	t := 0
	if entry != nil {
		t = entry.Timeout
	}
	if t == 0 && defaults != nil {
		t = defaults.Timeout
	}
	if t == 0 {
		t = DefaultTimeoutSeconds
	}
	if t > MaxTimeoutSeconds {
		t = MaxTimeoutSeconds
	}
	return t
}

// resolveStageTimeout picks the stage's own timeout, else the entry/defaults
// top-level timeout, else the package default, capped at the hard max.
func resolveStageTimeout(stage *EnvConfig, entry *MergedEntry) int {
	if stage != nil && stage.Timeout > 0 {
		if stage.Timeout > MaxTimeoutSeconds {
			return MaxTimeoutSeconds
		}
		return stage.Timeout
	}
	t := entry.Timeout
	if t == 0 {
		t = DefaultTimeoutSeconds
	}
	if t > MaxTimeoutSeconds {
		t = MaxTimeoutSeconds
	}
	return t
}

// normalizeTagList trims, lowercases and dedups entry tags.
func normalizeTagList(tags []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// MatchesTags reports whether an environment's tag set covers the entry tags
// (subset semantics; extra environment tags are fine).
func MatchesTags(envTags, entryTags []string) bool {
	have := map[string]bool{}
	for _, t := range envTags {
		have[strings.ToLower(strings.TrimSpace(t))] = true
	}
	for _, t := range entryTags {
		if !have[t] {
			return false
		}
	}
	return true
}

// PickEnvironment selects the environment to run an entry on, from the
// enabled candidates: the one whose tag set is the tightest superset of the
// entry tags (fewest unrelated tags), breaking ties by name. Returns nil when
// no candidate matches.
func PickEnvironment(entry MergedEntry, envs []store.TestEnvironment) *store.TestEnvironment {
	want := map[string]bool{}
	for _, t := range entry.Tags {
		want[t] = true
	}
	var best *store.TestEnvironment
	bestExtra := 0
	for i := range envs {
		env := &envs[i]
		if !MatchesTags(env.TagList(), entry.Tags) {
			continue
		}
		extra := 0
		for _, t := range env.TagList() {
			if !want[t] {
				extra++
			}
		}
		if best == nil || extra < bestExtra || (extra == bestExtra && env.Name < best.Name) {
			best = env
			bestExtra = extra
		}
	}
	return best
}

// SortedTagString joins tags with commas, display order.
func SortedTagString(tags []string) string {
	s := append([]string(nil), tags...)
	sort.Strings(s)
	return strings.Join(s, ",")
}
