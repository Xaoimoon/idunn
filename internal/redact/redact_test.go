package redact

import (
	"errors"
	"strings"
	"testing"
)

func TestURL(t *testing.T) {
	reset()
	AddURL("discord://s3cr3tT0ken@123456789012/webhooks?thread_id=998877")
	msg := String("POST https://discord.com/api/webhooks/123456789012/s3cr3tT0ken failed (thread 998877)")
	for _, leaked := range []string{"s3cr3tT0ken", "123456789012", "998877"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("%q leaked in %q", leaked, msg)
		}
	}
	if !strings.Contains(msg, "webhooks") {
		t.Errorf("plain words must stay readable: %q", msg)
	}
}

func TestError(t *testing.T) {
	reset()
	Add("hunter2-password")
	base := errors.New("auth failed for hunter2-password")
	err := Error(base)
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("secret leaked: %v", err)
	}
	if !errors.Is(err, base) {
		t.Error("redacted error must wrap the original")
	}
	if Error(nil) != nil {
		t.Error("Error(nil) must be nil")
	}
}

func TestShortValuesIgnored(t *testing.T) {
	reset()
	Add("abc")
	if String("abc") != "abc" {
		t.Error("short values are not secrets")
	}
}
