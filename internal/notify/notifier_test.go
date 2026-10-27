package notify

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nicholas-fedor/shoutrrr/pkg/types"
)

type recorder struct{ messages []string }

func (r *recorder) SendContext(_ context.Context, msg string, _ *types.Params) []error {
	r.messages = append(r.messages, msg)
	return nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const (
	oldD = "sha256:aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	newD = "sha256:bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222"
)

func sample() []Event {
	return []Event{
		{Kind: KindUpdated, Level: Info, Service: "web_app", Image: "nginx:1", OldDigest: oldD, NewDigest: newD, Duration: 42 * time.Second},
		{Kind: KindRollback, Level: Warn, Service: "db_api", Image: "api:2", State: "rollback_completed", Message: "healthcheck failed"},
	}
}

func TestRenderBuiltins(t *testing.T) {
	for lang, want := range map[string][]string{
		"en": {"**web_app** updated", "aaaaaaaaaaaa → bbbbbbbbbbbb", "42s", "rolled back", "healthcheck failed"},
		"fr": {"**web_app** mis à jour", "aaaaaaaaaaaa → bbbbbbbbbbbb", "42s", "revenu à l'image précédente"},
	} {
		tmpl, err := Parse(lang, "")
		if err != nil {
			t.Fatal(err)
		}
		n := NewWithSender(nil, tmpl, Info, true, quiet)
		msg, err := n.Render(sample())
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Errorf("%s: %q missing in\n%s", lang, w, msg)
			}
		}
	}
}

func TestEveryKindRenders(t *testing.T) {
	kinds := []Kind{KindStartup, KindUpdated, KindPending, KindDetected, KindRollback,
		KindRollbackFailed, KindPaused, KindGuard, KindHeadFailed, KindCleanupFailed, KindNewVersion}
	for _, lang := range []string{"en", "fr"} {
		tmpl, _ := Parse(lang, "")
		n := NewWithSender(nil, tmpl, Info, true, quiet)
		for _, k := range kinds {
			msg, err := n.Render([]Event{{Kind: k, Service: "s", Image: "i:1", Nodes: []string{"node1"}, ApplyAt: time.Now()}})
			if err != nil || msg == "" {
				t.Errorf("%s/%s: %q, %v", lang, k, msg, err)
			}
		}
	}
}

func TestCustomTemplate(t *testing.T) {
	tmpl, err := Parse("en", `{{ range .Events }}{{ .Service }}={{ short .NewDigest }};{{ end }}`)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := NewWithSender(nil, tmpl, Info, true, quiet).Render(sample())
	if msg != "web_app=bbbbbbbbbbbb;db_api=;" {
		t.Errorf("msg = %q", msg)
	}
	if _, err := Parse("en", "{{ .Broken"); err == nil {
		t.Error("invalid template must fail at startup")
	}
}

func TestSendModesAndLevels(t *testing.T) {
	tmpl, _ := Parse("en", "")

	r := &recorder{}
	NewWithSender(r, tmpl, Info, true, quiet).Send(context.Background(), sample())
	if len(r.messages) != 1 {
		t.Errorf("report mode: %d messages, want 1", len(r.messages))
	}

	r = &recorder{}
	NewWithSender(r, tmpl, Info, false, quiet).Send(context.Background(), sample())
	if len(r.messages) != 2 {
		t.Errorf("per-event mode: %d messages, want 2", len(r.messages))
	}

	r = &recorder{}
	NewWithSender(r, tmpl, Warn, true, quiet).Send(context.Background(), sample())
	if len(r.messages) != 1 || strings.Contains(r.messages[0], "web_app") {
		t.Errorf("warn level must drop info events: %v", r.messages)
	}

	r = &recorder{}
	NewWithSender(r, tmpl, Error, true, quiet).Send(context.Background(), sample())
	NewWithSender(r, tmpl, Info, true, quiet).Send(context.Background(), nil)
	if len(r.messages) != 0 {
		t.Errorf("nothing must be sent: %v", r.messages)
	}
}

func TestShortDigest(t *testing.T) {
	if ShortDigest(newD) != "bbbbbbbbbbbb" || ShortDigest("") != "" || ShortDigest("abc") != "abc" {
		t.Error("unexpected ShortDigest result")
	}
}
