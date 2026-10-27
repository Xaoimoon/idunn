// Package versions finds newer releases among the tags of an image whose tag
// is a version number.
package versions

import (
	"regexp"
	"strconv"
	"strings"
)

// Level is the size of a version bump. Levels are ordered: a minimum level
// keeps that bump and the larger ones.
type Level int

const (
	Patch Level = iota
	Minor
	Major
	None // never notify
)

func (l Level) String() string {
	switch l {
	case Minor:
		return "minor"
	case Major:
		return "major"
	case None:
		return "none"
	default:
		return "patch"
	}
}

// ParseLevel parses "patch", "minor", "major" or "none".
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "patch":
		return Patch, true
	case "minor":
		return Minor, true
	case "major":
		return Major, true
	case "none":
		return None, true
	}
	return Patch, false
}

// tagRE matches an optional "v", one to four numbers separated by dots and an
// optional suffix starting with a dash ("2.9-alpine", "v7.15.5", "16").
var tagRE = regexp.MustCompile(`^(v?)(\d+(?:\.\d+){0,3})(-[0-9A-Za-z][0-9A-Za-z._-]*)?$`)

// version is a parsed version tag.
type version struct {
	prefix string
	nums   []int
	suffix string
}

func parse(tag string) (version, bool) {
	m := tagRE.FindStringSubmatch(tag)
	if m == nil {
		return version{}, false
	}
	parts := strings.Split(m[2], ".")
	v := version{prefix: m[1], suffix: m[3], nums: make([]int, len(parts))}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, false
		}
		v.nums[i] = n
	}
	return v, true
}

// IsVersion reports whether tag is a version tag Iðunn can compare.
func IsVersion(tag string) bool {
	_, ok := parse(tag)
	return ok
}

// sameShape reports whether two versions can be compared: same prefix, same
// number of components and same suffix. Requiring the same suffix keeps
// variants apart ("-alpine" vs "-bookworm") and leaves out pre-releases
// ("-rc1", "-beta") of a plain version.
func sameShape(a, b version) bool {
	return a.prefix == b.prefix && a.suffix == b.suffix && len(a.nums) == len(b.nums)
}

// compare returns the bump from a to b and whether b is newer than a. Both
// must have the same shape.
func compare(a, b version) (Level, bool) {
	for i := range a.nums {
		if a.nums[i] == b.nums[i] {
			continue
		}
		if b.nums[i] < a.nums[i] {
			return 0, false
		}
		switch i {
		case 0:
			return Major, true
		case 1:
			return Minor, true
		default:
			return Patch, true
		}
	}
	return 0, false
}

// Newer returns the newest tag of the same shape as current whose bump is at
// least min, with the size of that bump. ok is false when there is none, or
// when current is not a version tag.
func Newer(current string, tags []string, min Level) (latest string, bump Level, ok bool) {
	cur, valid := parse(current)
	if !valid || min == None {
		return "", 0, false
	}
	var best version
	for _, t := range tags {
		v, valid := parse(t)
		if !valid || !sameShape(cur, v) {
			continue
		}
		lvl, newer := compare(cur, v)
		if !newer || lvl < min {
			continue
		}
		if ok {
			if _, better := compare(best, v); !better {
				continue
			}
		}
		best, latest, bump, ok = v, t, lvl, true
	}
	return latest, bump, ok
}
