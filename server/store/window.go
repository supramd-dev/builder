package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Scheduling windows: the daily time range in which a stage may *start*.
//
// A stage may be load-sensitive (a benchmark that wants an idle machine, a
// full-matrix run that would otherwise fight the interactive users), and the
// yaml says so with a window:
//
//	window: "22:00-06:00"
//
// The stage is then claimed only inside that range, every day; outside it the
// node simply stays pending and nothing else about it changes. A start that
// wraps over midnight is the normal case (22:00-06:00 runs through the small
// hours), an empty window means "any time", and a *running* stage is never
// interrupted when its window closes: the window gates the start, not the
// stage.
//
// The clock these are read against is the site timezone (the IANA zone of the
// site configuration, the one every displayed timestamp is rendered in), so
// the 22:00 an operator reads in the UI is the 22:00 the scheduler uses.
// Without a configured zone the server's own local time applies.

// Window is a parsed daily scheduling window: the minutes-of-day range
// [Start, End), where End < Start means the range wraps over midnight. The
// zero Window is "no window" — schedulable at any time — which is what an
// absent yaml field gives.
type Window struct {
	Start int // minutes since midnight, 0..1439
	End   int // minutes since midnight, 1..1440 (1440 = end of day)
}

// AnyTime reports whether the window places no restriction.
func (w Window) AnyTime() bool { return w.Start == 0 && w.End == 0 }

// String renders the window in the yaml spelling ("22:00-06:00"), or "" when
// it places no restriction. The parse-side round trip is deliberate: a
// message about a window prints what the operator wrote.
func (w Window) String() string {
	if w.AnyTime() {
		return ""
	}
	return formatClock(w.Start) + "-" + formatClock(w.End)
}

// OpenAt reports whether the window is open at t. The window is read in t's
// own location, so callers convert to the scheduling timezone first (see
// ParseWindow / SiteLocation).
func (w Window) OpenAt(t time.Time) bool {
	if w.AnyTime() {
		return true
	}
	min := t.Hour()*60 + t.Minute()
	if w.Start < w.End {
		return min >= w.Start && min < w.End
	}
	// Wrapping over midnight: the range is the evening tail plus the morning
	// head, and the two halves are disjoint.
	return min >= w.Start || min < w.End
}

// ParseWindow parses the yaml window spelling: "HH:MM-HH:MM", 24-hour clock,
// minute granularity. An empty string (or blanks) means "any time".
//
// A window whose start equals its end is refused rather than read as either
// "never" or "all day": both readings are surprising, and neither is what a
// typo'd "22:00-22:00" meant.
func ParseWindow(s string) (Window, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Window{}, nil
	}
	// The separator is a dash; a hyphen inside a time is impossible, so
	// splitting on the first one is unambiguous.
	start, end, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, fmt.Errorf("window %q must be \"HH:MM-HH:MM\"", s)
	}
	startMin, err := parseClock(start)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: %v", s, err)
	}
	endMin, err := parseClock(end)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: %v", s, err)
	}
	if startMin == 24*60 {
		// 24:00 spells the end of the day, so it is an end and never a start
		// (a start of 24:00 would read as midnight, written two ways).
		return Window{}, fmt.Errorf("window %q: 24:00 is the end of the day, not a start; write 00:00", s)
	}
	if startMin == endMin {
		return Window{}, fmt.Errorf("window %q starts and ends at the same minute, so it would never open", s)
	}
	return Window{Start: startMin, End: endMin}, nil
}

// parseClock parses "HH:MM" into minutes since midnight. 24:00 is accepted as
// the end of the day, so a window may run to midnight without wrapping.
//
// The two digits are required rather than merely tolerated ("6:0" is refused):
// the field is written by hand in a config file, and only one spelling of a
// time keeps the parse errors unambiguous.
func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	bad := func() (int, error) { return 0, fmt.Errorf("%q is not a HH:MM time", s) }
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return bad()
	}
	h, m = strings.TrimSpace(h), strings.TrimSpace(m)
	if len(h) != 2 || len(m) != 2 {
		return bad()
	}
	hour, err := strconv.Atoi(h)
	if err != nil {
		return bad()
	}
	minute, err := strconv.Atoi(m)
	if err != nil {
		return bad()
	}
	if hour == 24 && minute == 0 {
		return 24 * 60, nil
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("%q is not a time of day (00:00-24:00)", s)
	}
	return hour*60 + minute, nil
}

// formatClock renders minutes since midnight as HH:MM.
func formatClock(min int) string {
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

// SiteLocation resolves the timezone scheduling windows are read in: the site
// configuration's IANA zone when one is set, else the server's local zone. An
// unloadable zone falls back to local rather than failing the claim — a site
// whose zoneinfo is missing must still run its tests, just in the server's
// own time.
func (s *Store) SiteLocation() *time.Location {
	cfg, err := s.GetSiteConfig()
	if err != nil || cfg == nil || strings.TrimSpace(cfg.Timezone) == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(strings.TrimSpace(cfg.Timezone))
	if err != nil {
		return time.Local
	}
	return loc
}

// windowOpenAt reports whether a node's stored window (its raw yaml spelling)
// is open at now, read in loc. An unparsable window counts as open: the yaml
// is validated at dispatch, so anything else here is a hand-edited row, and
// parking it for ever would be worse than running it.
func windowOpenAt(raw string, now time.Time, loc *time.Location) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	w, err := ParseWindow(raw)
	if err != nil {
		return true
	}
	return w.OpenAt(now.In(loc))
}
