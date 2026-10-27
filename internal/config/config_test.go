package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func loadFrom(t *testing.T, vars map[string]string, files map[string]string) (*Config, error) {
	t.Helper()
	lookup := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	read := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("not found")
	}
	return load(lookup, read)
}

func TestDefaults(t *testing.T) {
	c, err := loadFrom(t, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule != DefaultSchedule || c.Interval != 0 {
		t.Errorf("schedule = %q, interval = %s", c.Schedule, c.Interval)
	}
	if c.LabelFilter.String() != "idunn.enable=true" || c.LabelIgnore.String() != "idunn.enable=false" {
		t.Errorf("labels = %s / %s", c.LabelFilter, c.LabelIgnore)
	}
	if !c.ClusterGuard || !c.RollbackOnFailure || !c.Cleanup || !c.NotificationReport {
		t.Error("boolean defaults must be true")
	}
	if c.MonitorOnly || c.DryRun || c.RunOnce {
		t.Error("monitor-only, dry-run and run-once must default to false")
	}
	if c.MinAge != 0 || c.UpdateTimeout != 10*time.Minute || c.MaxConcurrent != 1 || c.CleanupKeep != 1 {
		t.Errorf("unexpected numeric defaults: %+v", c)
	}
	if c.NotificationLang != "en" || c.NotificationLevel != "info" || c.HeadFailureWarn != HeadWarnAuto {
		t.Errorf("unexpected notification defaults: %+v", c)
	}
	if len(c.Blackout) != 0 || len(c.NotificationURLs) != 0 {
		t.Error("blackout and notification URLs must be empty by default")
	}
}

func TestCustomProfile(t *testing.T) {
	c, err := loadFrom(t, map[string]string{
		"IDUNN_LABEL_FILTER":          "autodeploy=true",
		"IDUNN_LABEL_IGNORE":          "autodeploy=false",
		"IDUNN_BLACKOUT":              "00:00-05:30",
		"IDUNN_MIN_AGE":               "6h",
		"IDUNN_NOTIFICATION_LANG":     "fr",
		"IDUNN_NOTIFICATION_URL_FILE": "/run/secrets/url",
	}, map[string]string{"/run/secrets/url": "discord://token@123456 generic://example.com\n"})
	if err != nil {
		t.Fatal(err)
	}
	if c.LabelFilter.Key != "autodeploy" || c.LabelFilter.Value != "true" {
		t.Errorf("filter = %+v", c.LabelFilter)
	}
	if c.MinAge != 6*time.Hour || c.NotificationLang != "fr" || len(c.Blackout) != 1 {
		t.Errorf("unexpected values: %+v", c)
	}
	if len(c.NotificationURLs) != 2 {
		t.Errorf("urls = %d, want 2", len(c.NotificationURLs))
	}
}

func TestInterval(t *testing.T) {
	c, err := loadFrom(t, map[string]string{"IDUNN_INTERVAL": "6h"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 6*time.Hour || c.Schedule != "" {
		t.Errorf("interval = %s, schedule = %q", c.Interval, c.Schedule)
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"schedule and interval": {"IDUNN_SCHEDULE": "0 0 * * * *", "IDUNN_INTERVAL": "1h"},
		"bad bool":              {"IDUNN_DRY_RUN": "maybe"},
		"bad duration":          {"IDUNN_MIN_AGE": "six hours"},
		"bad label":             {"IDUNN_LABEL_FILTER": "autodeploy"},
		"bad lang":              {"IDUNN_NOTIFICATION_LANG": "de"},
		"bad blackout":          {"IDUNN_BLACKOUT": "25:00-26:00"},
		"zero concurrency":      {"IDUNN_MAX_CONCURRENT": "0"},
		"value and file":        {"IDUNN_NOTIFICATION_URL": "x", "IDUNN_NOTIFICATION_URL_FILE": "/f"},
		"bad head warn":         {"IDUNN_HEAD_FAILURE_WARN": "sometimes"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadFrom(t, vars, nil); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestSecretFileErrorDoesNotLeak(t *testing.T) {
	_, err := loadFrom(t, map[string]string{"IDUNN_NOTIFICATION_URL_FILE": "/missing"}, nil)
	if err == nil || !strings.Contains(err.Error(), "/missing") {
		t.Fatalf("err = %v", err)
	}
}
