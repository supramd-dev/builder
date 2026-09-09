package runner

import (
	"strings"
	"testing"

	"md-builder/server/store"
)

const validYAML = `
version: 1
defaults:
  timeout: 3600
  env:
    OMP_NUM_THREADS: "4"
  build:
    generator: cmake
    cmake_flags: "-DCMAKE_BUILD_TYPE=Release"
    threads: 8
matrix:
  - tags: [CPU]
    description: Generic CPU build
    env:
      CC: gcc
      CXX: g++
    build:
      cmake_flags: "-DENABLE_MPI=OFF"
    unit:
      command: "ctest --test-dir build -L unit --output-on-failure"
      timeout: 600
    regression:
      command: "python3 run_regression.py --suite full"
      timeout: 1800
  - tags: [GPU, CUDA]
    env:
      CC: clang
    build:
      generator: script
      command: "./build.sh --cuda"
    unit:
      command: "ctest --test-dir build -L unit"
`

func TestParseConfigValid(t *testing.T) {
	entries, err := ParseConfig([]byte(validYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}

	cpu := entries[0]
	if got := strings.Join(cpu.Tags, ","); got != "cpu" {
		t.Errorf("cpu tags not normalized: %q", got)
	}
	if cpu.Unit.Timeout != 600 {
		t.Errorf("cpu unit timeout: want 600 (own), got %d", cpu.Unit.Timeout)
	}
	if cpu.Regression.Timeout != 1800 {
		t.Errorf("cpu regression timeout: want 1800 (own), got %d", cpu.Regression.Timeout)
	}
	if cpu.Build.CMakeFlags != "-DENABLE_MPI=OFF" {
		t.Errorf("cpu cmake flags should override defaults: %q", cpu.Build.CMakeFlags)
	}
	if cpu.Build.Generator != GeneratorCMake {
		t.Errorf("cpu generator should default to cmake: %q", cpu.Build.Generator)
	}
	if cpu.Env["OMP_NUM_THREADS"] != "4" || cpu.Env["CC"] != "gcc" {
		t.Errorf("cpu env merge wrong: %v", cpu.Env)
	}

	gpu := entries[1]
	if gpu.Build.Generator != GeneratorScript || gpu.Build.Command != "./build.sh --cuda" {
		t.Errorf("gpu build wrong: %+v", gpu.Build)
	}
	if gpu.Build.Threads != 8 {
		t.Errorf("gpu threads should come from defaults: %d", gpu.Build.Threads)
	}
	if gpu.Unit.Timeout != 3600 {
		t.Errorf("gpu unit timeout should fall back to defaults: want 3600, got %d", gpu.Unit.Timeout)
	}
	if gpu.Regression != nil {
		t.Errorf("gpu regression should be nil")
	}
}

func TestParseConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"no version", "matrix:\n- tags: [cpu]\n  unit:\n    command: x", "version"},
		{"wrong version", "version: 2\nmatrix:\n- tags: [cpu]\n  unit:\n    command: x", "version"},
		{"empty matrix", "version: 1\nmatrix: []", "empty"},
		{"missing matrix", "version: 1", "empty"},
		{"no tags", "version: 1\nmatrix:\n- unit:\n    command: x", "tags"},
		{"no stages", "version: 1\nmatrix:\n- tags: [cpu]", "neither"},
		{"unit no command", "version: 1\nmatrix:\n- tags: [cpu]\n  unit:\n    timeout: 10", "command"},
		{"bad generator", "version: 1\nmatrix:\n- tags: [cpu]\n  build:\n    generator: make\n  unit:\n    command: x", "generator"},
		{"script without command", "version: 1\nmatrix:\n- tags: [cpu]\n  build:\n    generator: script\n  unit:\n    command: x", "build.command"},
		{"duplicate tags", "version: 1\nmatrix:\n- tags: [cpu]\n  unit:\n    command: x\n- tags: [CPU]\n  regression:\n    command: y", "duplicate"},
		{"bad yaml", "version: 1\nmatrix: [unclosed", "parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestMatchesTags(t *testing.T) {
	if !MatchesTags([]string{"cpu", "mpi"}, []string{"cpu"}) {
		t.Error("superset should match")
	}
	if MatchesTags([]string{"cpu"}, []string{"cpu", "mpi"}) {
		t.Error("subset should not match")
	}
	if MatchesTags([]string{"gpu"}, []string{"cpu"}) {
		t.Error("disjoint should not match")
	}
	if MatchesTags(nil, []string{"cpu"}) {
		t.Error("no env tags should not match")
	}
}

func envTags(name string, tags string, enabled bool) store.TestEnvironment {
	return store.TestEnvironment{Name: name, Tags: tags, Enabled: enabled}
}

func TestPickEnvironment(t *testing.T) {
	entry := MergedEntry{Tags: []string{"cpu"}}

	// No match at all.
	envs := []store.TestEnvironment{envTags("gpu-a", "gpu,cuda", true)}
	if PickEnvironment(entry, envs) != nil {
		t.Error("no matching environment should return nil")
	}

	// Tightest match wins: "cpu-node" (cpu only) beats "big-node" (cpu+mpi).
	envs = []store.TestEnvironment{
		envTags("big-node", "cpu,mpi", true),
		envTags("cpu-node", "cpu", true),
	}
	pick := PickEnvironment(entry, envs)
	if pick == nil || pick.Name != "cpu-node" {
		t.Errorf("want cpu-node, got %v", pick)
	}

	// Tie broken by name.
	envs = []store.TestEnvironment{
		envTags("beta", "cpu,mpi", true),
		envTags("alpha", "cpu,mpi", true),
	}
	if pick := PickEnvironment(entry, envs); pick == nil || pick.Name != "alpha" {
		t.Errorf("want alpha (name order), got %v", pick)
	}

	// Disabled environments are not eligible (caller filters, but
	// defensively the picker itself works on what it gets).
	envs = []store.TestEnvironment{envTags("cpu-node", "cpu", true)}
	if pick := PickEnvironment(entry, envs); pick == nil || pick.Name != "cpu-node" {
		t.Errorf("want cpu-node, got %v", pick)
	}
}
