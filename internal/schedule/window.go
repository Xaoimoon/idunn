// Package schedule handles blackout windows, the time ranges in which no
// update may start.
package schedule

import (
	"fmt"
	"strings"
	"time"
)

// Window is a daily (or weekly, when Day is set) time range. Start and End are
// minutes after midnight, both inclusive. A window whose End is before its
// Start runs past midnight into the next day.
type Window struct {
	Day   *time.Weekday
	Start int
	End   int
	raw   string
}

func (w Window) String() string { return w.raw }

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseWindows parses a comma-separated list such as
// "01:30-05:30,Sat 22:00-23:59".
func ParseWindows(s string) ([]Window, error) {
	var out []Window
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		w, err := parseWindow(part)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func parseWindow(s string) (Window, error) {
	w := Window{raw: s}
	rng := s
	if day, rest, ok := strings.Cut(s, " "); ok {
		d, known := weekdays[strings.ToLower(day)[:min(3, len(day))]]
		if !known {
			return w, fmt.Errorf("window %q: unknown day %q", s, day)
		}
		w.Day = &d
		rng = strings.TrimSpace(rest)
	}
	from, to, ok := strings.Cut(rng, "-")
	if !ok {
		return w, fmt.Errorf("window %q: expected HH:MM-HH:MM", s)
	}
	var err error
	if w.Start, err = parseClock(from); err != nil {
		return w, fmt.Errorf("window %q: %w", s, err)
	}
	if w.End, err = parseClock(to); err != nil {
		return w, fmt.Errorf("window %q: %w", s, err)
	}
	return w, nil
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid time %q", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Contains reports whether t falls inside the window, in t's location.
func (w Window) Contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	today := t.Weekday()
	onDay := func(d time.Weekday) bool { return w.Day == nil || *w.Day == d }
	if w.Start <= w.End {
		return onDay(today) && m >= w.Start && m <= w.End
	}
	// Past midnight: the evening part belongs to the window's day, the
	// morning part to the following day.
	yesterday := (today + 6) % 7
	return (onDay(today) && m >= w.Start) || (onDay(yesterday) && m <= w.End)
}

// InBlackout returns the first window containing t, if any.
func InBlackout(windows []Window, t time.Time) (Window, bool) {
	for _, w := range windows {
		if w.Contains(t) {
			return w, true
		}
	}
	return Window{}, false
}
