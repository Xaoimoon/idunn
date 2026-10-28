// Package notify renders pass events with text/template and sends them
// through Shoutrrr.
package notify

import (
	"strings"
	"time"
)

// Level orders events by severity.
type Level int

const (
	Info Level = iota
	Warn
	Error
)

func (l Level) String() string {
	switch l {
	case Warn:
		return "warn"
	case Error:
		return "error"
	default:
		return "info"
	}
}

// ParseLevel parses "info", "warn" or "error".
func ParseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "warn", "warning":
		return Warn
	case "error":
		return Error
	default:
		return Info
	}
}

// Kind identifies an event type. Templates switch on it.
type Kind string

const (
	KindStartup        Kind = "startup"
	KindUpdated        Kind = "updated"
	KindPending        Kind = "pending"
	KindDetected       Kind = "detected"
	KindRollback       Kind = "rollback"
	KindRollbackFailed Kind = "rollback_failed"
	KindPaused         Kind = "paused"
	KindGuard          Kind = "guard"
	KindHeadFailed     Kind = "head_failed"
	KindCleanupFailed  Kind = "cleanup_failed"
	KindNewVersion     Kind = "new_version"
	KindSelfUpdate     Kind = "self_update"
	KindSelfFailed     Kind = "self_update_failed"
	KindRetrySkipped   Kind = "retry_skipped"
)

// Event is one notable outcome of a pass. Fields not relevant to a Kind stay
// empty.
type Event struct {
	Kind    Kind
	Level   Level
	Time    time.Time
	Service string
	Image   string

	OldDigest string
	NewDigest string
	Duration  time.Duration
	ApplyAt   time.Time // pending: when the image becomes eligible

	State   string // Swarm update state
	Message string // Swarm or task error message

	Registry   string
	StatusCode int

	Nodes []string // guard: "name (state/availability)"; cleanup: node name

	// New version fields: newest tag available and size of the bump from
	// the running tag ("patch", "minor" or "major").
	Tag  string
	Bump string

	// Startup fields.
	Version  string
	Node     string
	Schedule string
	Blackout string
	Tracked  int
	Mode     string
}

// ShortDigest returns the first 12 hex characters of a digest.
func ShortDigest(d string) string {
	if _, hex, ok := strings.Cut(d, ":"); ok {
		d = hex
	}
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
