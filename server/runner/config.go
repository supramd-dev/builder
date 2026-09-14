// Package runner implements the single-job execution pipeline: parse the
// md-builder.yaml test matrix from the code repository, generate the remote
// bash script, execute it over SSH and turn the line-protocol report into
// database rows.
package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"md-builder/server/store"
)

// ConfigVersion is the only supported md-builder.yaml schema version.
const ConfigVersion = 2

// MaxTimeoutSeconds is the hard per-command timeout cap (4h).
const MaxTimeoutSeconds = 4 * 3600

// DefaultTimeoutSeconds applies when neither defaults nor the entry set one.
const DefaultTimeoutSeconds = 3600

// EnvConfig is a test stage: unit, or one regression preset.
type EnvConfig struct {
	Command     CommandList  `yaml:"command" json:"command"`
	Description string       `yaml:"description,omitempty" json:"description,omitempty"` // preset label (regression presets)
	Workdir     string       `yaml:"workdir,omitempty" json:"workdir,omitempty"`         // relative to the code dir; empty = code dir
	Timeout     int          `yaml:"timeout,omitempty" json:"timeout,omitempty"`         // seconds; 0 = use default
	Results     ResultsPaths `yaml:"results,omitempty" json:"results,omitempty"`         // googletest results files (XML/JSON) produced by the command
}

// CommandList is a stage command that may be one command or several. The
// yaml form accepts a scalar string or a list of strings; the commands run
// in order and stop at the first failure — a non-zero exit skips the
// remaining commands and fails the stage with that exit code. A
// multi-line scalar (yaml `|` block) stays a single command.
//
//	command: "ctest -L unit"
//	command: ["make data", "ctest -L unit"]
type CommandList []string

// UnmarshalYAML accepts a scalar command or a list of commands.
func (c *CommandList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Value == "" && node.Tag == "!!null" {
			*c = nil
			return nil
		}
		*c = CommandList{node.Value}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := node.Decode(&list); err != nil {
			return err
		}
		*c = CommandList(list)
		return nil
	default:
		return fmt.Errorf("command must be a string or a list of strings")
	}
}

// UnmarshalJSON accepts a single string or a list (legacy snapshots stored
// one string).
func (c *CommandList) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" || s == "" {
		*c = nil
		return nil
	}
	if s[0] == '[' {
		var list []string
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*c = CommandList(list)
		return nil
	}
	var one string
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	*c = CommandList{one}
	return nil
}

// MarshalJSON emits the list form (one command → a one-element list keeps
// the shape stable).
func (c CommandList) MarshalJSON() ([]byte, error) {
	list := []string(c)
	if list == nil {
		list = []string{}
	}
	return json.Marshal(list)
}

// Clean returns the commands trimmed, with empties dropped (a list entry
// may be an empty line from yaml block scalars).
func (c CommandList) Clean() CommandList {
	out := make(CommandList, 0, len(c))
	for _, s := range c {
		if v := strings.TrimSpace(s); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// String joins the commands with " && " for contexts that need one line
// (display, the manual-dispatch default).
func (c CommandList) String() string {
	return strings.Join(c.Clean(), " && ")
}

// IsEmpty reports whether no non-blank command is set.
func (c CommandList) IsEmpty() bool {
	return len(c.Clean()) == 0
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

// BuildConfig describes the build stage of an entry.
type BuildConfig struct {
	Command string `yaml:"command,omitempty" json:"command,omitempty"` // the build command
	Workdir string `yaml:"workdir,omitempty" json:"workdir,omitempty"` // command workdir; empty = code dir
}

// RegressionUse is the matrix entry's regression stanza: it references
// presets by name instead of inlining commands. Empty use = every preset;
// disable drops named cases from the used set.
type RegressionUse struct {
	Use     []string `yaml:"use,omitempty" json:"use,omitempty"`
	Disable []string `yaml:"disable,omitempty" json:"disable,omitempty"`
}

// EntryConfig is one matrix entry, before defaults are merged in.
type EntryConfig struct {
	Tags        []string          `yaml:"tags" json:"tags"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Build       BuildConfig       `yaml:"build,omitempty" json:"build,omitempty"`
	Unit        *EnvConfig        `yaml:"unit,omitempty" json:"unit,omitempty"`
	Regression  *RegressionUse    `yaml:"regression,omitempty" json:"regression,omitempty"`
	Timeout     int               `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// rawConfig mirrors md-builder.yaml before defaults merging.
type rawConfig struct {
	Version  int                   `yaml:"version"`
	Defaults *EntryConfig          `yaml:"defaults,omitempty"`
	Presets  map[string]*EnvConfig `yaml:"presets,omitempty"` // shared regression cases
	Matrix   []EntryConfig         `yaml:"matrix"`
}

// RegressionCase is one effective regression case of a merged entry: a
// preset selected by the entry's use/disable, resolved to its final command,
// workdir, timeout and results files. Each becomes its own sub-task.
type RegressionCase struct {
	Name        string       `json:"name"` // preset name
	Description string       `json:"description,omitempty"`
	Command     CommandList  `json:"command"`
	Workdir     string       `json:"workdir,omitempty"`
	Timeout     int          `json:"timeout"`
	Results     ResultsPaths `json:"results,omitempty"`
}

// MergedEntry is an entry with defaults applied — the config snapshot stored
// on a job and used to generate the remote script.
type MergedEntry struct {
	Tags        []string          `json:"tags"`
	Description string            `json:"description,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Build       BuildConfig       `json:"build"`
	Unit        *EnvConfig        `json:"unit,omitempty"`
	Regression  []RegressionCase  `json:"regression,omitempty"`
	Timeout     int               `json:"timeout"`
}

// ParseConfig parses and validates md-builder.yaml. The returned entries are
// defaults-merged and ready for environment matching.
func ParseConfig(data []byte) ([]MergedEntry, error) {
	// Strict decoding: unknown fields (e.g. the removed build.generator /
	// cmake_flags / threads) must fail loudly instead of being ignored.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse md-builder.yaml: %w", err)
	}
	if raw.Version != ConfigVersion {
		return nil, fmt.Errorf("md-builder.yaml: version must be %d, got %d", ConfigVersion, raw.Version)
	}
	if len(raw.Matrix) == 0 {
		return nil, fmt.Errorf("md-builder.yaml: matrix must not be empty")
	}
	for name, p := range raw.Presets {
		if p == nil || p.Command.IsEmpty() {
			return nil, fmt.Errorf("md-builder.yaml: preset %q needs a command", name)
		}
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
		if entry.Unit != nil && entry.Unit.Command.IsEmpty() {
			return nil, fmt.Errorf("md-builder.yaml: matrix entry %d has unit without a command", i+1)
		}
		if strings.TrimSpace(entry.Build.Command) == "" && (raw.Defaults == nil || strings.TrimSpace(raw.Defaults.Build.Command) == "") {
			return nil, fmt.Errorf("md-builder.yaml: matrix entry %d: build requires a command (entry or defaults.build)", i+1)
		}

		tags := normalizeTagList(entry.Tags)
		key := strings.Join(tags, ",")
		if seen[key] {
			return nil, fmt.Errorf("md-builder.yaml: duplicate matrix entry tags [%s]", key)
		}
		seen[key] = true

		merged, err := mergeDefaults(raw.Defaults, raw.Presets, &entry, tags)
		if err != nil {
			return nil, err
		}
		if merged.Unit == nil && len(merged.Regression) == 0 {
			return nil, fmt.Errorf("md-builder.yaml: matrix entry %d resolves to no stage (unit missing, regression selects no preset)", i+1)
		}
		out = append(out, merged)
	}
	return out, nil
}

// expandRegression resolves the entry's use/disable against the presets into
// the effective ordered case list. Empty use selects every preset (name
// order for determinism); disable drops named cases from the selection.
func expandRegression(presets map[string]*EnvConfig, use *RegressionUse, defaults *EntryConfig) ([]RegressionCase, error) {
	if use == nil {
		return nil, nil
	}

	names := use.Use
	if len(names) == 0 {
		for name := range presets {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	disabled := map[string]bool{}
	for _, d := range use.Disable {
		disabled[strings.TrimSpace(d)] = true
	}

	var cases []RegressionCase
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if disabled[name] {
			continue
		}
		p, ok := presets[name]
		if !ok || p == nil {
			return nil, fmt.Errorf("md-builder.yaml: regression.use references unknown preset %q", name)
		}
		c := RegressionCase{
			Name:        name,
			Description: p.Description,
			Command:     p.Command,
			Workdir:     p.Workdir,
			Timeout:     p.Timeout,
			Results:     p.Results,
		}
		if c.Timeout == 0 {
			c.Timeout = defaultTimeoutFor(defaults, nil)
		}
		if c.Timeout > MaxTimeoutSeconds {
			c.Timeout = MaxTimeoutSeconds
		}
		cases = append(cases, c)
	}
	// disable entries naming a preset NOT in the used set are still valid
	// (they are no-ops), but a disable of an unknown name hints at a typo.
	for _, d := range use.Disable {
		d = strings.TrimSpace(d)
		if _, ok := presets[d]; !ok && d != "" {
			return nil, fmt.Errorf("md-builder.yaml: regression.disable references unknown preset %q", d)
		}
	}
	return cases, nil
}

// mergeDefaults applies defaults onto an entry: env maps are merged key-wise
// (entry wins), scalars are taken from the entry when set, and the
// regression use/disable is expanded against the presets.
func mergeDefaults(defaults *EntryConfig, presets map[string]*EnvConfig, entry *EntryConfig, tags []string) (MergedEntry, error) {
	m := MergedEntry{
		Tags:        tags,
		Description: entry.Description,
		Unit:        entry.Unit,
	}
	if m.Unit != nil {
		c := *m.Unit
		m.Unit = &c
	}
	if m.Unit != nil && m.Unit.Workdir == "" && defaults != nil && defaults.Unit != nil {
		m.Unit.Workdir = defaults.Unit.Workdir
	}
	if m.Unit != nil && m.Unit.Timeout == 0 {
		m.Unit.Timeout = defaultTimeoutFor(defaults, entry)
	}

	var err error
	m.Regression, err = expandRegression(presets, entry.Regression, defaults)
	if err != nil {
		return MergedEntry{}, err
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
		if build.Command == "" {
			build.Command = defaults.Build.Command
		}
		if build.Workdir == "" {
			build.Workdir = defaults.Build.Workdir
		}
	}
	m.Build = build
	return m, nil
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
// sec is the stage's own resolved timeout (0 = fall through).
func resolveStageTimeout(sec int, entry *MergedEntry) int {
	if sec > 0 {
		if sec > MaxTimeoutSeconds {
			return MaxTimeoutSeconds
		}
		return sec
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
