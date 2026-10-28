// Package config reads Iðunn's settings from IDUNN_* environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xaoimoon/idunn/internal/schedule"
	"github.com/xaoimoon/idunn/internal/versions"
)

// DefaultSchedule runs a pass at the top of every hour.
const DefaultSchedule = "0 0 * * * *"

// DefaultVersionSchedule runs the version check every day at 09:00.
const DefaultVersionSchedule = "0 0 9 * * *"

// HeadFailureWarn controls when a failed manifest HEAD is reported.
type HeadFailureWarn string

const (
	HeadWarnNever  HeadFailureWarn = "never"
	HeadWarnAuto   HeadFailureWarn = "auto"
	HeadWarnAlways HeadFailureWarn = "always"
)

// Label is a key=value pair matched against service labels.
type Label struct {
	Key   string
	Value string
}

func (l Label) String() string { return l.Key + "=" + l.Value }

// Config holds every runtime setting.
type Config struct {
	Schedule string
	Interval time.Duration
	Blackout []schedule.Window

	ClusterGuard bool
	LabelFilter  Label
	LabelIgnore  Label

	MinAge      time.Duration
	MonitorOnly bool
	DryRun      bool
	RunOnce     bool

	RollbackOnFailure bool
	SelfUpdate        bool
	UpdateTimeout     time.Duration
	MaxConcurrent     int

	Cleanup     bool
	CleanupKeep int

	VersionCheck    bool
	VersionSchedule string
	VersionLevel    versions.Level

	RegistryAuthFile string
	HeadFailureWarn  HeadFailureWarn

	NotificationURLs     []string
	NotificationLang     string
	NotificationTemplate string
	NotificationReport   bool
	NotificationLevel    string

	LogLevel  string
	LogFormat string

	MetricsAddr string
}

// Load reads the configuration from the environment.
func Load() (*Config, error) {
	return load(os.LookupEnv, os.ReadFile)
}

type lookupFunc func(string) (string, bool)
type readFunc func(string) ([]byte, error)

func load(lookup lookupFunc, read readFunc) (*Config, error) {
	e := &env{lookup: lookup, read: read}
	c := &Config{
		ClusterGuard:       e.bool("IDUNN_CLUSTER_GUARD", true),
		MinAge:             e.duration("IDUNN_MIN_AGE", 0),
		MonitorOnly:        e.bool("IDUNN_MONITOR_ONLY", false),
		DryRun:             e.bool("IDUNN_DRY_RUN", false),
		RunOnce:            e.bool("IDUNN_RUN_ONCE", false),
		RollbackOnFailure:  e.bool("IDUNN_ROLLBACK_ON_FAILURE", true),
		SelfUpdate:         e.bool("IDUNN_SELF_UPDATE", false),
		UpdateTimeout:      e.duration("IDUNN_UPDATE_TIMEOUT", 10*time.Minute),
		MaxConcurrent:      e.int("IDUNN_MAX_CONCURRENT", 1),
		Cleanup:            e.bool("IDUNN_CLEANUP", true),
		CleanupKeep:        e.int("IDUNN_CLEANUP_KEEP", 1),
		VersionCheck:       e.bool("IDUNN_VERSION_CHECK", false),
		VersionSchedule:    e.string("IDUNN_VERSION_SCHEDULE", DefaultVersionSchedule),
		RegistryAuthFile:   e.string("IDUNN_REGISTRY_AUTH_FILE", ""),
		HeadFailureWarn:    HeadFailureWarn(strings.ToLower(e.string("IDUNN_HEAD_FAILURE_WARN", string(HeadWarnAuto)))),
		NotificationLang:   strings.ToLower(e.string("IDUNN_NOTIFICATION_LANG", "en")),
		NotificationReport: e.bool("IDUNN_NOTIFICATION_REPORT", true),
		NotificationLevel:  strings.ToLower(e.string("IDUNN_NOTIFICATION_LEVEL", "info")),
		LogLevel:           strings.ToLower(e.string("IDUNN_LOG_LEVEL", "info")),
		LogFormat:          strings.ToLower(e.string("IDUNN_LOG_FORMAT", "text")),
		MetricsAddr:        e.string("IDUNN_METRICS_ADDR", ""),
	}

	sched, schedSet := e.lookupNonEmpty("IDUNN_SCHEDULE")
	c.Interval = e.duration("IDUNN_INTERVAL", 0)
	switch {
	case schedSet && c.Interval > 0:
		e.fail("IDUNN_SCHEDULE and IDUNN_INTERVAL are mutually exclusive")
	case c.Interval > 0:
	case schedSet:
		c.Schedule = sched
	default:
		c.Schedule = DefaultSchedule
	}
	if c.Interval < 0 {
		e.fail("IDUNN_INTERVAL must be positive")
	}

	if v := e.string("IDUNN_BLACKOUT", ""); v != "" {
		w, err := schedule.ParseWindows(v)
		if err != nil {
			e.fail("IDUNN_BLACKOUT: %v", err)
		}
		c.Blackout = w
	}

	lvl := e.string("IDUNN_VERSION_LEVEL", "patch")
	if l, ok := versions.ParseLevel(lvl); ok && l != versions.None {
		c.VersionLevel = l
	} else {
		e.fail("IDUNN_VERSION_LEVEL: %q must be one of patch, minor, major", lvl)
	}

	c.LabelFilter = e.label("IDUNN_LABEL_FILTER", "idunn.enable=true")
	c.LabelIgnore = e.label("IDUNN_LABEL_IGNORE", "idunn.enable=false")

	if urls := e.secret("IDUNN_NOTIFICATION_URL"); urls != "" {
		c.NotificationURLs = strings.Fields(urls)
	}
	c.NotificationTemplate = e.secret("IDUNN_NOTIFICATION_TEMPLATE")

	if c.MinAge < 0 {
		e.fail("IDUNN_MIN_AGE must not be negative")
	}
	if c.UpdateTimeout <= 0 {
		e.fail("IDUNN_UPDATE_TIMEOUT must be positive")
	}
	if c.MaxConcurrent < 1 {
		e.fail("IDUNN_MAX_CONCURRENT must be at least 1")
	}
	if c.CleanupKeep < 0 {
		e.fail("IDUNN_CLEANUP_KEEP must not be negative")
	}
	e.oneOf("IDUNN_HEAD_FAILURE_WARN", string(c.HeadFailureWarn), "never", "auto", "always")
	e.oneOf("IDUNN_NOTIFICATION_LANG", c.NotificationLang, "en", "fr")
	e.oneOf("IDUNN_NOTIFICATION_LEVEL", c.NotificationLevel, "info", "warn", "error")
	e.oneOf("IDUNN_LOG_LEVEL", c.LogLevel, "debug", "info", "warn", "error")
	e.oneOf("IDUNN_LOG_FORMAT", c.LogFormat, "text", "json")

	if len(e.errs) > 0 {
		return nil, errors.Join(e.errs...)
	}
	return c, nil
}

// env collects parse errors so that all of them are reported at once.
type env struct {
	lookup lookupFunc
	read   readFunc
	errs   []error
}

func (e *env) fail(format string, args ...any) {
	e.errs = append(e.errs, fmt.Errorf(format, args...))
}

func (e *env) lookupNonEmpty(key string) (string, bool) {
	v, ok := e.lookup(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (e *env) string(key, def string) string {
	if v, ok := e.lookupNonEmpty(key); ok {
		return v
	}
	return def
}

func (e *env) bool(key string, def bool) bool {
	v, ok := e.lookupNonEmpty(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.fail("%s: %q is not a boolean", key, v)
		return def
	}
	return b
}

func (e *env) int(key string, def int) int {
	v, ok := e.lookupNonEmpty(key)
	if !ok {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		e.fail("%s: %q is not an integer", key, v)
		return def
	}
	return i
}

func (e *env) duration(key string, def time.Duration) time.Duration {
	v, ok := e.lookupNonEmpty(key)
	if !ok {
		return def
	}
	if v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.fail("%s: %q is not a duration (e.g. 30m, 6h)", key, v)
		return def
	}
	return d
}

func (e *env) label(key, def string) Label {
	v := e.string(key, def)
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		e.fail("%s: %q must be key=value", key, v)
		return Label{}
	}
	return Label{Key: strings.TrimSpace(k), Value: strings.TrimSpace(val)}
}

// secret reads KEY, or the file named by KEY_FILE. The value is never echoed
// in error messages.
func (e *env) secret(key string) string {
	v, hasValue := e.lookupNonEmpty(key)
	path, hasFile := e.lookupNonEmpty(key + "_FILE")
	switch {
	case hasValue && hasFile:
		e.fail("%s and %s_FILE are mutually exclusive", key, key)
		return ""
	case hasFile:
		b, err := e.read(path)
		if err != nil {
			e.fail("%s_FILE: cannot read %s", key, path)
			return ""
		}
		return strings.TrimSpace(string(b))
	default:
		return v
	}
}

func (e *env) oneOf(key, v string, allowed ...string) {
	for _, a := range allowed {
		if v == a {
			return
		}
	}
	e.fail("%s: %q must be one of %s", key, v, strings.Join(allowed, ", "))
}
