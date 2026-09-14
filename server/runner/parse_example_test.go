package runner

import (
	"os"
	"strings"
	"testing"
)

func TestParseExampleYAML(t *testing.T) {
	data, err := os.ReadFile("../../md-builder.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("example must parse: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	// Entry 1: heat+poisson, unit ctest, gcc env, in-source cmake build.
	e1 := entries[0]
	if len(e1.Regression) != 2 || e1.Regression[0].Name != "heat" || e1.Regression[1].Name != "poisson" {
		t.Fatalf("entry 1 cases wrong: %+v", e1.Regression)
	}
	if e1.Regression[0].Description == "" {
		t.Error("preset description should be carried into the case")
	}
	if e1.Regression[0].Workdir != "regression/heat" || e1.Regression[0].Timeout != 1800 {
		t.Errorf("heat case wrong: %+v", e1.Regression[0])
	}
	if got := e1.Regression[0].Command.String(); got != "make prepare && ./run_heat.py --nt 500 --tol 1e-5" {
		t.Errorf("heat list command wrong: %q", got)
	}
	if e1.Regression[1].Timeout != 3600 {
		t.Errorf("poisson should fall back to defaults timeout, got %d", e1.Regression[1].Timeout)
	}
	if e1.Unit == nil || !strings.Contains(e1.Unit.Command.String(), "ctest") {
		t.Fatalf("entry 1 unit wrong: %+v", e1.Unit)
	}
	if e1.Unit.Timeout != 600 {
		t.Errorf("unit timeout should be 600, got %d", e1.Unit.Timeout)
	}
	if e1.Build.Workdir != "" || !strings.Contains(e1.Build.Command, "ENABLE_MPI=OFF") {
		t.Errorf("entry 1 build wrong: %+v", e1.Build)
	}
	if e1.Env["CC"] != "gcc" || e1.Env["OMP_NUM_THREADS"] != "4" {
		t.Errorf("entry 1 env merge wrong: %v", e1.Env)
	}

	// Entry 2: build in a workdir, all three presets.
	e2 := entries[1]
	if e2.Build.Workdir != "build" || !strings.Contains(e2.Build.Command, "ENABLE_MPI=ON") {
		t.Errorf("entry 2 build wrong: %+v", e2.Build)
	}
	if len(e2.Regression) != 3 {
		t.Fatalf("entry 2 should run 3 cases, got %d", len(e2.Regression))
	}
	if e2.Env["OMP_NUM_THREADS"] != "1" {
		t.Errorf("entry env should override defaults env: %v", e2.Env)
	}

	// Entry 3: script build, all presets minus heat.
	e3 := entries[2]
	if e3.Build.Command != "$MD_CODE_DIR/build.sh --cuda -j8" || e3.Build.Workdir != "build" {
		t.Errorf("entry 3 build wrong: %+v", e3.Build)
	}
	if len(e3.Regression) != 2 || e3.Regression[0].Name != "eos-table" || e3.Regression[1].Name != "poisson" {
		t.Fatalf("entry 3 cases wrong: %+v", e3.Regression)
	}
	if e3.Regression[0].Timeout != 600 {
		t.Errorf("eos-table timeout should be its own 600, got %d", e3.Regression[0].Timeout)
	}
}
