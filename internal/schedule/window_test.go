package schedule

import (
	"testing"
	"time"
)

func at(day time.Weekday, hhmm string) time.Time {
	// 2026-01-04 is a Sunday.
	base := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(day))
	t, _ := time.Parse("15:04", hhmm)
	return base.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute)
}

func TestWindows(t *testing.T) {
	ws, err := ParseWindows("00:00-05:30, Sat 22:00-23:59, fri 23:00-01:00")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at(time.Monday, "00:00"), true},
		{at(time.Monday, "05:30"), true},
		{at(time.Monday, "05:31"), false},
		{at(time.Monday, "12:00"), false},
		{at(time.Saturday, "22:30"), true},
		{at(time.Sunday, "22:30"), false},
		{at(time.Friday, "23:30"), true},   // evening part of the Friday window
		{at(time.Saturday, "00:45"), true}, // morning part, next day (also in 00:00-05:30)
		{at(time.Thursday, "23:30"), false},
	}
	for _, c := range cases {
		if _, got := InBlackout(ws, c.t); got != c.want {
			t.Errorf("%s %s: got %v, want %v", c.t.Weekday(), c.t.Format("15:04"), got, c.want)
		}
	}
}

func TestMidnightOnlyWindow(t *testing.T) {
	ws, err := ParseWindows("Fri 23:00-01:00")
	if err != nil {
		t.Fatal(err)
	}
	if _, in := InBlackout(ws, at(time.Saturday, "00:30")); !in {
		t.Error("Saturday 00:30 belongs to the Friday window")
	}
	if _, in := InBlackout(ws, at(time.Sunday, "00:30")); in {
		t.Error("Sunday 00:30 is outside the window")
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{"01:00", "Xyz 01:00-02:00", "1h-2h", "24:00-01:00"} {
		if _, err := ParseWindows(s); err == nil {
			t.Errorf("%q: expected an error", s)
		}
	}
}
