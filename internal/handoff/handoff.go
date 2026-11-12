// Package handoff builds the summary the next person on call reads at the
// start of a shift: what fired during the window, what is still live at the
// handover instant, and which manual overrides are about to apply.
//
// Everything is derived from the store and the workspace config, never from
// "now", so running it twice over the same window prints the same bytes and
// two consecutive handoffs diff cleanly.
package handoff

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/config"
	"github.com/moveeeax/telegram-oncall/internal/rota"
	"github.com/moveeeax/telegram-oncall/internal/store"
)

// Window is the half-open shift interval [Since, Until).
type Window struct {
	Since time.Time
	Until time.Time
}

// ShiftWindow returns the window of the shift that ends at until for the named
// rota: from the start of the period that was running just before until, up to
// until itself. Asking at the exact handover instant therefore yields the shift
// that is being handed over, not the one that is starting.
func ShiftWindow(cfg *config.Config, rotaName string, until time.Time) (Window, error) {
	a, err := rota.NewResolver(cfg).OnCall(rotaName, until.Add(-time.Second))
	if err != nil {
		return Window{}, err
	}
	return Window{Since: a.PeriodStart, Until: until}, nil
}

// Shift is who hands a rota over to whom at the end of the window.
type Shift struct {
	Rota     string
	Outgoing config.Responder
	Incoming config.Responder
	// IncomingUntil is when the incoming responder's period ends.
	IncomingUntil time.Time
	// Reason explains the incoming assignment (rotation slot or override).
	Reason string
}

// Counts tallies ledger actions inside the window.
type Counts struct {
	Opened, Held, Deduped, Pages, Escalations, Acks, Resolves int
}

// Group collapses every firing of one (route, fingerprint) in the window into
// a single line, so a flapping check does not fill the page.
type Group struct {
	Route       string
	Fingerprint string
	Title       string
	Severity    string
	Incidents   int // distinct incidents that fired in the window
	Firings     int // open, held and dedup events in the window
	Flapping    bool
	// LastStatus is the status, at the end of the window, of the most recent
	// incident in the group.
	LastStatus store.Status
}

// Live is an incident carried forward into the next shift.
type Live struct {
	ID         int64
	Route      string
	Title      string
	Severity   string
	Status     store.Status // as of the end of the window
	Since      time.Time
	AckedBy    string
	HeldReason string
}

// Override is a manual rota override in effect at, or starting during, the
// incoming shift.
type Override struct {
	Rota      string
	Responder string
	From, To  time.Time
	Reason    string
}

// Report is everything a handoff renders.
type Report struct {
	Workspace string
	Window    Window
	Shifts    []Shift
	Counts    Counts
	Fired     []Group
	Live      []Live
	Overrides []Override
}

// Options selects what Build looks at. Config may be nil, in which case the
// report has no rota or override sections. An empty Rota means every rota in
// the config.
type Options struct {
	Config *config.Config
	Rota   string
	Window Window
}

// Build assembles a Report from the store.
func Build(ctx context.Context, st *store.Store, opt Options) (Report, error) {
	w := Window{Since: opt.Window.Since.UTC().Truncate(time.Second), Until: opt.Window.Until.UTC().Truncate(time.Second)}
	if !w.Since.Before(w.Until) {
		return Report{}, fmt.Errorf("handoff: window start %s is not before its end %s",
			w.Since.Format(time.RFC3339), w.Until.Format(time.RFC3339))
	}
	rep := Report{Window: w}

	if opt.Config != nil {
		rep.Workspace = opt.Config.Workspace
		if err := addRotas(&rep, opt.Config, opt.Rota); err != nil {
			return Report{}, err
		}
	}

	entries, err := st.LedgerBetween(ctx, w.Since, w.Until)
	if err != nil {
		return Report{}, err
	}
	incidents := map[int64]store.Incident{}
	load := func(id int64) (store.Incident, error) {
		if inc, ok := incidents[id]; ok {
			return inc, nil
		}
		inc, err := st.Incident(ctx, id)
		if err != nil {
			return store.Incident{}, err
		}
		incidents[id] = inc
		return inc, nil
	}

	type key struct{ route, fp string }
	groups := map[key]*Group{}
	latest := map[key]store.Incident{}
	seen := map[key]map[int64]bool{}
	for _, e := range entries {
		switch e.Action {
		case "open":
			rep.Counts.Opened++
		case "held":
			rep.Counts.Held++
		case "dedup":
			rep.Counts.Deduped++
		case "page":
			rep.Counts.Pages++
		case "escalate":
			rep.Counts.Escalations++
		case "ack":
			rep.Counts.Acks++
		case "resolve":
			rep.Counts.Resolves++
		}
		if e.Action != "open" && e.Action != "held" && e.Action != "dedup" {
			continue
		}
		inc, err := load(e.IncidentID)
		if err != nil {
			return Report{}, err
		}
		k := key{inc.Route, inc.Fingerprint}
		g := groups[k]
		if g == nil {
			g = &Group{Route: inc.Route, Fingerprint: inc.Fingerprint}
			groups[k] = g
			seen[k] = map[int64]bool{}
		}
		g.Firings++
		if !seen[k][inc.ID] {
			seen[k][inc.ID] = true
			g.Incidents++
		}
		g.Flapping = g.Flapping || inc.Flapping
		if prev, ok := latest[k]; !ok || inc.ID > prev.ID {
			latest[k] = inc
		}
	}
	for k, g := range groups {
		inc := latest[k]
		g.Title, g.Severity = inc.Title, inc.Severity
		g.LastStatus = StatusAt(inc, w.Until)
		rep.Fired = append(rep.Fired, *g)
	}
	sort.Slice(rep.Fired, func(i, j int) bool {
		a, b := rep.Fired[i], rep.Fired[j]
		if ra, rb := SeverityRank(a.Severity), SeverityRank(b.Severity); ra != rb {
			return ra < rb
		}
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		return a.Fingerprint < b.Fingerprint
	})

	live, err := st.IncidentsLiveAt(ctx, w.Until)
	if err != nil {
		return Report{}, err
	}
	for _, inc := range live {
		l := Live{
			ID: inc.ID, Route: inc.Route, Title: inc.Title, Severity: inc.Severity,
			Status: StatusAt(inc, w.Until), Since: inc.CreatedAt, HeldReason: inc.HeldReason,
		}
		if l.Status == store.StatusAcked {
			l.AckedBy = inc.AckedBy
		}
		rep.Live = append(rep.Live, l)
	}
	return rep, nil
}

func addRotas(rep *Report, cfg *config.Config, only string) error {
	names := []string{only}
	if only == "" {
		names = names[:0]
		for _, r := range cfg.Rotas {
			names = append(names, r.Name)
		}
	}
	res := rota.NewResolver(cfg)
	until := rep.Window.Until
	for _, n := range names {
		out, err := res.OnCall(n, until.Add(-time.Second))
		if err != nil {
			return err
		}
		in, err := res.OnCall(n, until)
		if err != nil {
			return err
		}
		rep.Shifts = append(rep.Shifts, Shift{
			Rota: n, Outgoing: out.Responder, Incoming: in.Responder,
			IncomingUntil: in.PeriodEnd, Reason: in.Reason,
		})
		rt, _ := cfg.Rota(n)
		for _, o := range rt.Overrides {
			// Anything that overlaps the incoming period is state the next
			// person inherits, whether it is already running or starts later.
			if o.To.After(until) && o.From.Before(in.PeriodEnd) {
				rep.Overrides = append(rep.Overrides, Override{
					Rota: n, Responder: o.Responder, From: o.From.UTC(), To: o.To.UTC(), Reason: o.Reason,
				})
			}
		}
	}
	sort.SliceStable(rep.Overrides, func(i, j int) bool {
		return rep.Overrides[i].From.Before(rep.Overrides[j].From)
	})
	return nil
}

// StatusAt reconstructs an incident's status at instant t from its timestamps,
// so a handoff for a past window does not depend on what happened afterwards.
// The incident must have been created before t.
func StatusAt(inc store.Incident, t time.Time) store.Status {
	switch {
	case inc.ResolvedAt != nil && inc.ResolvedAt.Before(t):
		return store.StatusResolved
	case inc.AckedAt != nil && inc.AckedAt.Before(t):
		return store.StatusAcked
	case inc.HeldReason != "":
		return store.StatusHeld
	default:
		return store.StatusOpen
	}
}

// SeverityRank orders severities most urgent first; unknown ones sort last.
func SeverityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	case "info":
		return 2
	default:
		return 3
	}
}
