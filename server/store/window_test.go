package store

import (
	"testing"
	"time"
)

// TestParseWindow pins the yaml spelling: "HH:MM-HH:MM", 24-hour clock, and
// an empty value meaning "any time".
func TestParseWindow(t *testing.T) {
	cases := []struct {
		in    string
		start int
		end   int
	}{
		{"", 0, 0},
		{"   ", 0, 0},
		{"22:00-06:00", 22 * 60, 6 * 60}, // the nightly window, wrapping
		{"09:30-17:45", 9*60 + 30, 17*60 + 45},
		{"00:00-24:00", 0, 24 * 60},       // the whole day, spelled out
		{"06:00-06:01", 6 * 60, 6*60 + 1}, // one minute
		{" 22:00 - 06:00 ", 22 * 60, 6 * 60},
	}
	for _, c := range cases {
		got, err := ParseWindow(c.in)
		if err != nil {
			t.Errorf("ParseWindow(%q) errored: %v", c.in, err)
			continue
		}
		if got.Start != c.start || got.End != c.end {
			t.Errorf("ParseWindow(%q) = {%d, %d}, want {%d, %d}", c.in, got.Start, got.End, c.start, c.end)
		}
	}
}

// TestParseWindowRejects covers the values that must fail the dispatch rather
// than quietly schedule the stage at some other time.
func TestParseWindowRejects(t *testing.T) {
	for _, in := range []string{
		"22:00",          // no range at all
		"22:00-",         // missing end
		"-06:00",         // missing start
		"22:00-22:00",    // never opens: refused rather than read as any time
		"25:00-06:00",    // no such hour
		"22:60-06:00",    // no such minute
		"22:00-6:0",      // not HH:MM
		"6:00-7:00",      // the digits are required, not merely tolerated
		"22.00-06.00",    // wrong separator
		"十点-六点",          // not a clock
		"24:30-06:00",    // 24:30 is past the end of the day
		"24:00-06:00",    // 24:00 is an end of the day, never a start
		"22:00-06:00-07", // more than one range
	} {
		if _, err := ParseWindow(in); err == nil {
			t.Errorf("ParseWindow(%q) accepted an invalid window", in)
		}
	}
}

// TestWindowRoundTrip pins that the parser and the printer agree, so a
// message about a window quotes what the operator wrote.
func TestWindowRoundTrip(t *testing.T) {
	for _, in := range []string{"22:00-06:00", "09:30-17:45", "06:00-06:01"} {
		w, err := ParseWindow(in)
		if err != nil {
			t.Fatalf("ParseWindow(%q): %v", in, err)
		}
		if got := w.String(); got != in {
			t.Errorf("ParseWindow(%q).String() = %q", in, got)
		}
	}
	if got := (Window{}).String(); got != "" {
		t.Errorf("the zero window prints as %q, want \"\"", got)
	}
}

// TestWindowOpenAt pins the edges of the range: the start minute is inside,
// the end minute is not, and the wrapping window is the evening tail plus the
// morning head.
func TestWindowOpenAt(t *testing.T) {
	night, err := ParseWindow("22:00-06:00")
	if err != nil {
		t.Fatal(err)
	}
	day, err := ParseWindow("09:30-17:45")
	if err != nil {
		t.Fatal(err)
	}
	whole, err := ParseWindow("00:00-24:00")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time {
		return time.Date(2026, 10, 6, h, m, 0, 0, time.UTC)
	}
	cases := []struct {
		w    Window
		when time.Time
		want bool
	}{
		{night, at(22, 0), true},    // the start minute is inside
		{night, at(21, 59), false},  //
		{night, at(23, 59), true},   // the evening tail
		{night, at(0, 0), true},     // and the morning head after midnight
		{night, at(5, 59), true},    //
		{night, at(6, 0), false},    // the end minute is not: the range is half-open
		{night, at(12, 0), false},   //
		{day, at(9, 30), true},      //
		{day, at(9, 29), false},     //
		{day, at(17, 44), true},     //
		{day, at(17, 45), false},    //
		{day, at(22, 0), false},     // a same-day window does not wrap
		{whole, at(0, 0), true},     //
		{whole, at(23, 59), true},   //
		{Window{}, at(3, 0), true},  // no window: any time
		{Window{}, at(13, 0), true}, //
	}
	for _, c := range cases {
		if got := c.w.OpenAt(c.when); got != c.want {
			t.Errorf("window %q at %s = %v, want %v", c.w.String(), c.when.Format("15:04"), got, c.want)
		}
	}
}

// TestWindowOpenAtReadsTheGivenLocation pins that a window is read in the
// timezone of the time it is handed: the same instant is inside the window in
// one zone and outside it in another, which is what makes the site timezone
// the operator's.
func TestWindowOpenAtReadsTheGivenLocation(t *testing.T) {
	w, err := ParseWindow("22:00-06:00")
	if err != nil {
		t.Fatal(err)
	}
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	// 2026-10-06 15:00 UTC is 23:00 in Shanghai (inside) and 15:00 in UTC.
	instant := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)
	if !w.OpenAt(instant.In(shanghai)) {
		t.Error("23:00 in the site zone should be inside the nightly window")
	}
	if w.OpenAt(instant.In(time.UTC)) {
		t.Error("15:00 UTC should be outside the nightly window")
	}
}

// TestClaimReadyTaskHoldsAWindowedNodeUntilItsWindowOpens is the scheduling
// contract: a node whose stage may only start inside its window is left
// pending outside it — while the other ready nodes are still handed out — and
// is claimed by the first tick that falls inside the window.
func TestClaimReadyTaskHoldsAWindowedNodeUntilItsWindowOpens(t *testing.T) {
	s := newTestTaskStore(t)
	// Pin the scheduling zone, so the test does not depend on the machine's
	// local time: windows are read in the site timezone.
	if _, err := s.GetSiteConfig(); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSiteConfig(&SiteConfig{Timezone: "UTC"}, "timezone"); err != nil {
		t.Fatal(err)
	}

	env, commit := newGraphFixture(t, s, "window")
	node := func(kind, name, window string) *Task {
		return &Task{
			Kind: kind, NodeKey: kind, Name: name,
			CommitID: commit.ID, EnvironmentID: env.ID, Config: "{}", Window: window,
		}
	}
	// clone → { performance (any time), unit (nightly only) }.
	stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), []TaskNode{
		{Task: node(TaskKindClone, "clone repositories", "")},
		{Task: node(TaskKindPerformance, "performance tests", "22:00-06:00"), Deps: []int64{TaskSubPlaceholderBase + 0}},
		{Task: node(TaskKindUnit, "unit tests", ""), Deps: []int64{TaskSubPlaceholderBase + 0}},
	})
	if err != nil {
		t.Fatalf("dispatch graph: %v", err)
	}
	// stored is [root, clone, performance, unit].
	perf, unit := stored[2], stored[3]
	if got := reloadTask(t, s, perf.ID).Window; got != "22:00-06:00" {
		t.Fatalf("the windowed node stored window %q", got)
	}

	noon := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	night := time.Date(2026, 10, 6, 23, 0, 0, 0, time.UTC)

	// clone runs at any hour and is claimed first: the graph's entry node is
	// never windowed.
	got, err := s.claimReadyTaskAt(noon)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Kind != TaskKindClone {
		t.Fatalf("at noon the scheduler claimed %+v, want the clone node", got)
	}
	reportTask(t, s, got.ID, StatusPassed, "cloned")

	// Both dependents are ready now. At noon only the unwindowed one is
	// handed out — and it is not blocked behind the waiting node.
	got, err = s.claimReadyTaskAt(noon)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != unit.ID {
		t.Fatalf("at noon the scheduler claimed %+v, want the unit node", got)
	}
	reportTask(t, s, got.ID, StatusPassed, "unit ok")

	// Nothing else may start: the performance stage is waiting for tonight.
	if got, err := s.claimReadyTaskAt(noon); err != nil {
		t.Fatal(err)
	} else if got != nil {
		t.Fatalf("at noon the scheduler claimed %+v, want nothing (the performance window is closed)", got)
	}
	if got := reloadTask(t, s, perf.ID); got.Status != StatusPending {
		t.Fatalf("the waiting node is %s, want pending", got.Status)
	}

	// The first tick inside the window claims it, with no other change.
	got, err = s.claimReadyTaskAt(night)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != perf.ID {
		t.Fatalf("at 23:00 the scheduler claimed %+v, want the performance node", got)
	}
}

// TestClaimReadyTaskRunsAnUnparsableWindowAnyway pins the fallback for a row
// the yaml validation could not have produced (hand-edited, or written by an
// older schema): a window nobody can read must not park a stage for ever.
func TestClaimReadyTaskRunsAnUnparsableWindowAnyway(t *testing.T) {
	s := newTestTaskStore(t)
	if got, err := s.ClaimReadyTask(); err != nil {
		t.Fatal(err)
	} else if got != nil {
		t.Fatalf("empty store claimed %+v", got)
	}
	env, commit := newGraphFixture(t, s, "garbage-window")
	stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), []TaskNode{
		{Task: &Task{
			Kind: TaskKindUnit, NodeKey: TaskKindUnit, Name: "unit tests",
			CommitID: commit.ID, EnvironmentID: env.ID, Config: "{}",
			Window: "whenever the machine is free",
		}},
	})
	if err != nil {
		t.Fatalf("dispatch graph: %v", err)
	}
	got, err := s.claimReadyTaskAt(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != stored[1].ID {
		t.Fatalf("claimed %+v, want the node with the unreadable window", got)
	}
}

// TestSiteLocationFallsBackToLocal pins that a missing or unloadable site
// timezone never fails scheduling: the server's own zone is the fallback.
func TestSiteLocationFallsBackToLocal(t *testing.T) {
	s := newTestTaskStore(t)
	if _, err := s.GetSiteConfig(); err != nil {
		t.Fatal(err)
	}
	if got := s.SiteLocation(); got != time.Local {
		t.Errorf("with no timezone configured the location is %v, want local", got)
	}
	if err := s.UpdateSiteConfig(&SiteConfig{Timezone: "Asia/Shanghai"}, "timezone"); err != nil {
		t.Fatal(err)
	}
	if got := s.SiteLocation().String(); got != "Asia/Shanghai" {
		t.Errorf("location = %q, want Asia/Shanghai", got)
	}
	// A zone the server cannot load (no tzdata on this host, a hand-edited
	// row) degrades to local instead of stalling every windowed stage.
	if err := s.UpdateSiteConfig(&SiteConfig{Timezone: "Not/AZone"}, "timezone"); err != nil {
		t.Fatal(err)
	}
	if got := s.SiteLocation(); got != time.Local {
		t.Errorf("with an unloadable zone the location is %v, want local", got)
	}
}
