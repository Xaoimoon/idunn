package versions

import "testing"

func TestNewer(t *testing.T) {
	tags := []string{
		"latest", "12", "12.3", "12.3.2", "12.3.3", "12.3.4", "12.4.0", "13.0.0", "13.0.0-rc1",
		"12.3.4-noble", "13.0.1-ubi", "2.9-alpine", "2.10-alpine", "3.2-alpine", "3.2",
		"v7.15.5", "v7.16.0", "7.17.0", "16", "17", "17-rootless",
	}
	cases := []struct {
		current string
		min     Level
		latest  string
		bump    Level
		ok      bool
	}{
		{"12.3.3", Patch, "13.0.0", Major, true},
		{"12.3.3", Major, "13.0.0", Major, true},
		{"13.0.0", Patch, "", 0, false},       // 13.0.0-rc1 is a pre-release, 13.0.1-ubi a variant
		{"12.3.4-noble", Patch, "", 0, false}, // no newer -noble tag
		{"2.9-alpine", Patch, "3.2-alpine", Major, true},
		{"2.9-alpine", None, "", 0, false},
		{"v7.15.5", Patch, "v7.16.0", Minor, true}, // 7.17.0 has no "v"
		{"v7.15.5", Major, "", 0, false},
		{"16", Patch, "17", Major, true},
		{"12.3", Patch, "", 0, false}, // 12.4 does not exist, 3.2 is older
		{"latest", Patch, "", 0, false},
	}
	for _, c := range cases {
		latest, bump, ok := Newer(c.current, tags, c.min)
		if latest != c.latest || ok != c.ok || (ok && bump != c.bump) {
			t.Errorf("Newer(%q, %s) = %q, %s, %v; want %q, %s, %v", c.current, c.min, latest, bump, ok, c.latest, c.bump, c.ok)
		}
	}
}

func TestNewerMinimumLevelSkipsSmallerBumps(t *testing.T) {
	latest, bump, ok := Newer("1.2.3", []string{"1.2.4", "1.3.0", "1.3.1"}, Minor)
	if !ok || latest != "1.3.1" || bump != Minor {
		t.Errorf("got %q %s %v, want 1.3.1 minor", latest, bump, ok)
	}
	if _, _, ok := Newer("1.2.3", []string{"1.2.4", "1.2.10"}, Minor); ok {
		t.Error("patch releases must be ignored at minimum level minor")
	}
}

func TestIsVersion(t *testing.T) {
	for tag, want := range map[string]bool{
		"1": true, "v1.2.3": true, "2.9-alpine": true, "1.2.3.4": true,
		"latest": false, "alpine": false, "1.2.3.4.5": false, "1.2.3rc1": false, "sha-abc": false,
	} {
		if IsVersion(tag) != want {
			t.Errorf("IsVersion(%q) = %v", tag, !want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for s, want := range map[string]Level{"patch": Patch, "Minor": Minor, "major": Major, "none": None} {
		if l, ok := ParseLevel(s); !ok || l != want {
			t.Errorf("ParseLevel(%q) = %s, %v", s, l, ok)
		}
	}
	if _, ok := ParseLevel("huge"); ok {
		t.Error("unknown level accepted")
	}
}
