// Package routing decides which target and escalation policy an alert belongs
// to, and whether quiet hours suppress it.
package routing

import (
	"fmt"
	"strings"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/config"
	"github.com/moveeeax/telegram-oncall/internal/ingest"
)

// Decision is the result of routing one alert.
type Decision struct {
	Route      string
	Target     config.Target
	Escalation string
	Severity   string
	// Suppressed is true when quiet hours hold the page back. The incident is
	// still recorded; it is simply not paged.
	Suppressed bool
	Reason     string
}

// Router evaluates a config's routes against alerts.
type Router struct{ cfg *config.Config }

// New returns a Router bound to cfg.
func New(cfg *config.Config) *Router { return &Router{cfg: cfg} }

// Route returns every decision matching the alert, in config order. Routes stop
// at the first match unless that route sets `continue: true`. An alert that
// matches nothing returns an empty slice — that is a routing gap, not an error,
// and `oncall route` reports it loudly.
func (r *Router) Route(a ingest.Alert, now time.Time) []Decision {
	var out []Decision
	for _, rt := range r.cfg.Routes {
		if !matches(rt.Match, a) {
			continue
		}
		tgt, _ := r.cfg.Target(rt.Target)
		d := Decision{
			Route:      rt.Name,
			Target:     tgt,
			Escalation: rt.Escalation,
			Severity:   a.Severity(),
		}
		d.Suppressed, d.Reason = r.quiet(a.Severity(), now)
		out = append(out, d)
		if !rt.Continue {
			break
		}
	}
	return out
}

// quiet reports whether quiet hours suppress a severity at time now.
func (r *Router) quiet(severity string, now time.Time) (bool, string) {
	q := r.cfg.QuietHours
	if q == nil {
		return false, ""
	}
	if contains(q.Breakthrough, severity) {
		return false, fmt.Sprintf("severity %q breaks through quiet hours", severity)
	}
	if !contains(q.Suppress, severity) {
		return false, ""
	}
	loc, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return false, ""
	}
	if !inWindow(now.In(loc), q.From, q.To) {
		return false, ""
	}
	return true, fmt.Sprintf("quiet hours %s-%s %s suppress severity %q",
		q.From, q.To, q.Timezone, severity)
}

// InQuietHours reports whether local time t falls inside the configured window,
// ignoring severity. Exported for the CLI's status output.
func (r *Router) InQuietHours(t time.Time) bool {
	q := r.cfg.QuietHours
	if q == nil {
		return false
	}
	loc, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return false
	}
	return inWindow(t.In(loc), q.From, q.To)
}

// inWindow handles windows that wrap past midnight (23:00 -> 08:00).
func inWindow(local time.Time, from, to string) bool {
	f, err1 := parseHHMM(from)
	t, err2 := parseHHMM(to)
	if err1 != nil || err2 != nil {
		return false
	}
	cur := local.Hour()*60 + local.Minute()
	if f == t {
		return false
	}
	if f < t {
		return cur >= f && cur < t
	}
	return cur >= f || cur < t
}

func parseHHMM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

func matches(m config.Match, a ingest.Alert) bool {
	for k, want := range m.Labels {
		if a.Labels[k] != want {
			return false
		}
	}
	for k, expr := range m.LabelsRe {
		if !config.MustCompileLabelRe(expr).MatchString(a.Labels[k]) {
			return false
		}
	}
	for _, k := range m.Absent {
		if _, ok := a.Labels[k]; ok {
			return false
		}
	}
	if len(m.Severity) > 0 && !contains(m.Severity, a.Severity()) {
		return false
	}
	if len(m.SourceIn) > 0 && !contains(m.SourceIn, a.Source) {
		return false
	}
	return true
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}
