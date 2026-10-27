package notify

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/xaoimoon/idunn/internal/redact"
)

//go:embed templates/*.tmpl
var builtin embed.FS

// Sender delivers a rendered message; it is satisfied by Shoutrrr's router.
type Sender interface {
	SendContext(ctx context.Context, message string, params *types.Params) []error
}

// Notifier filters, renders and sends events.
type Notifier struct {
	sender   Sender
	tmpl     *template.Template
	minLevel Level
	report   bool
	log      *slog.Logger
}

// Options configures a Notifier.
type Options struct {
	URLs     []string
	Lang     string
	Template string // overrides the built-in template when set
	Level    Level
	Report   bool
}

// New builds a notifier. With no URL it only renders to the debug log.
func New(opts Options, log *slog.Logger) (*Notifier, error) {
	tmpl, err := Parse(opts.Lang, opts.Template)
	if err != nil {
		return nil, err
	}
	n := &Notifier{tmpl: tmpl, minLevel: opts.Level, report: opts.Report, log: log}
	if len(opts.URLs) > 0 {
		for _, u := range opts.URLs {
			redact.AddURL(u)
		}
		r, err := shoutrrr.CreateSender(opts.URLs...)
		if err != nil {
			return nil, fmt.Errorf("notification URL: %w", redact.Error(err))
		}
		n.sender = r
	}
	return n, nil
}

// NewWithSender is for tests.
func NewWithSender(s Sender, tmpl *template.Template, level Level, report bool, log *slog.Logger) *Notifier {
	return &Notifier{sender: s, tmpl: tmpl, minLevel: level, report: report, log: log}
}

var funcs = template.FuncMap{
	"short": ShortDigest,
	"dur": func(d time.Duration) string {
		return d.Round(time.Second).String()
	},
	"clock": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format("2006-01-02 15:04 MST")
	},
	"join": strings.Join,
}

// Parse returns the custom template if given, else the built-in one for lang.
func Parse(lang, custom string) (*template.Template, error) {
	t := template.New("notification").Funcs(funcs)
	if custom != "" {
		parsed, err := t.Parse(custom)
		if err != nil {
			return nil, fmt.Errorf("notification template: %w", err)
		}
		return parsed, nil
	}
	if lang != "fr" {
		lang = "en"
	}
	b, err := builtin.ReadFile("templates/" + lang + ".tmpl")
	if err != nil {
		return nil, err
	}
	return t.Parse(string(b))
}

// Data is what templates receive.
type Data struct {
	Events []Event
}

// Render renders events with the notifier's template.
func (n *Notifier) Render(events []Event) (string, error) {
	var buf bytes.Buffer
	if err := n.tmpl.Execute(&buf, Data{Events: events}); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

// Send notifies the events at or above the configured level: one message for
// all of them in report mode, one per event otherwise. Nothing is sent when no
// event qualifies.
func (n *Notifier) Send(ctx context.Context, events []Event) {
	var kept []Event
	for _, e := range events {
		if e.Level >= n.minLevel {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return
	}
	if n.report {
		n.send(ctx, kept)
		return
	}
	for _, e := range kept {
		n.send(ctx, []Event{e})
	}
}

func (n *Notifier) send(ctx context.Context, events []Event) {
	msg, err := n.Render(events)
	if err != nil {
		n.log.Error("cannot render notification", "error", err)
		return
	}
	if msg == "" {
		return
	}
	if n.sender == nil {
		n.log.Debug("notification (no URL configured)", "message", msg)
		return
	}
	params := types.Params{}
	params.SetTitle("Iðunn")
	for _, err := range n.sender.SendContext(ctx, msg, &params) {
		if err != nil {
			n.log.Error("notification failed", "error", redact.Error(err))
		}
	}
}
