package runner

import (
	"encoding/json"
	"testing"
	"time"
)

// The ExecResult/Result durations cross the wire as milliseconds. A
// time.Duration field would serialize as nanoseconds while the JSON tag
// promises durationMilliSeconds — a regression check (the Run command page
// once showed "1214909125 ms" for a 1.2s script).
func TestExecResultDurationMillis(t *testing.T) {
	res := ExecResult{
		Success:        true,
		ExitCode:       0,
		DurationMillis: 1234,
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	got, ok := decoded["durationMilliSeconds"].(float64)
	if !ok || got != 1234 {
		t.Fatalf("durationMilliSeconds = %v, want 1234 (ms, not ns)", decoded["durationMilliSeconds"])
	}
}

func TestResultDurationMillis(t *testing.T) {
	res := Result{Success: true, DurationMillis: 55 * time.Millisecond.Milliseconds()}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if got, _ := decoded["durationMilliSeconds"].(float64); got != 55 {
		t.Fatalf("durationMilliSeconds = %v, want 55", decoded["durationMilliSeconds"])
	}
}
