// Package escalate is the escalation state machine: the only real IP in this
// product.
//
// Design rules it holds to, because they are the ones on-call teams actually
// get burned by:
//
//   - Timers live in the database, never on a goroutine. Restarting the process
//     mid-incident loses nothing; the next Tick fires whatever came due.
//   - Every ladder step delivers exactly once, enforced by a unique index on
//     (incident_id, step) rather than by application bookkeeping.
//   - Acking cancels pending timers, and a second ack is a no-op.
//   - Held incidents (quiet hours, flapping) are recorded and ackable but never
//     paged, so the audit trail is complete even when the phone stays quiet.
package escalate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/clock"
	"github.com/moveeeax/telegram-oncall/internal/config"
	"github.com/moveeeax/telegram-oncall/internal/ingest"
	"github.com/moveeeax/telegram-oncall/internal/notify"
	"github.com/moveeeax/telegram-oncall/internal/rota"
	"github.com/moveeeax/telegram-oncall/internal/routing"
	"github.com/moveeeax/telegram-oncall/internal/store"
)

// Engine drives incidents through their escalation policy.
type Engine struct {
	cfg      *config.Config
	st       *store.Store
	router   *routing.Router
	resolver *rota.Resolver
	notifier notify.Notifier
	clk      clock.Clock
}

// New builds an Engine.
func New(cfg *config.Config, st *store.Store, n notify.Notifier, clk clock.Clock) *Engine {
	return &Engine{
		cfg:      cfg,
		st:       st,
		router:   routing.New(cfg),
		resolver: rota.NewResolver(cfg),
		notifier: n,
		clk:      clk,
	}
}

// Outcome describes what happened to one alert on ingest.
type Outcome struct {
	IncidentID int64
	Route      string
	Action     string // opened | deduped | held | resolved | unrouted
	Reason     string
}

// Ingest applies one normalised alert: routes it, dedups it, and either opens
// an incident with a durable step-0 timer or records it as held.
func (e *Engine) Ingest(ctx context.Context, a ingest.Alert) ([]Outcome, error) {
	now := e.clk.Now()
	decisions := e.router.Route(a, now)
	if len(decisions) == 0 {
		return []Outcome{{Action: "unrouted",
			Reason: "no route matched; add a catch-all route or this alert is silently lost"}}, nil
	}

	out := make([]Outcome, 0, len(decisions))
	for _, d := range decisions {
		if a.Status == ingest.StatusResolved {
			o, err := e.resolveByFingerprint(ctx, a, d, now)
			if err != nil {
				return nil, err
			}
			out = append(out, o)
			continue
		}
		o, err := e.open(ctx, a, d, now)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (e *Engine) resolveByFingerprint(ctx context.Context, a ingest.Alert, d routing.Decision, now time.Time) (Outcome, error) {
	inc, err := e.st.LiveIncident(ctx, a.Fingerprint, d.Route)
	if err != nil {
		// Nothing live to resolve; a resolved alert for an unknown fingerprint
		// is normal after a restart or a manual resolve.
		return Outcome{Route: d.Route, Action: "resolved", Reason: "no live incident"}, nil
	}
	ok, err := e.st.Resolve(ctx, inc.ID, "source", now)
	if err != nil {
		return Outcome{}, err
	}
	if _, err := e.st.CancelTimers(ctx, inc.ID); err != nil {
		return Outcome{}, err
	}
	if ok {
		if err := e.st.AppendLedger(ctx, inc.ID, now, "source", "resolve",
			"resolved by "+a.Source); err != nil {
			return Outcome{}, err
		}
		if inc.Status != store.StatusHeld {
			if err := e.send(ctx, notify.Page{
				IncidentID: inc.ID, Kind: notify.KindResolve, Step: inc.Step,
				Target: e.target(inc.Target), Title: inc.Title, Severity: inc.Severity,
				Fingerprint: inc.Fingerprint, At: now, Note: "resolved by " + a.Source,
			}); err != nil {
				return Outcome{}, err
			}
		}
	}
	return Outcome{IncidentID: inc.ID, Route: d.Route, Action: "resolved"}, nil
}

func (e *Engine) open(ctx context.Context, a ingest.Alert, d routing.Decision, now time.Time) (Outcome, error) {
	firings, err := e.st.RecordFiring(ctx, a.Fingerprint, now, e.cfg.Dedup.FlapWindow.D())
	if err != nil {
		return Outcome{}, err
	}
	flapping := firings > e.cfg.Dedup.FlapThreshold

	status := store.StatusOpen
	held := ""
	switch {
	case flapping:
		status, held = store.StatusHeld, fmt.Sprintf(
			"flapping: %d firings in %s (threshold %d)",
			firings, e.cfg.Dedup.FlapWindow, e.cfg.Dedup.FlapThreshold)
	case d.Suppressed:
		status, held = store.StatusHeld, d.Reason
	}

	inc, dedup, err := e.st.CreateIncident(ctx, store.Incident{
		Fingerprint: a.Fingerprint, Route: d.Route, Escalation: d.Escalation,
		Target: d.Target.Name, Title: a.Title, Severity: d.Severity,
		Status: status, Flapping: flapping, Labels: a.Labels,
		HeldReason: held, CreatedAt: now,
	})
	if err != nil {
		return Outcome{}, err
	}
	if dedup {
		if err := e.st.AppendLedger(ctx, inc.ID, now, "system", "dedup",
			fmt.Sprintf("occurrence %d for fingerprint %s", inc.Occurrences, a.Fingerprint)); err != nil {
			return Outcome{}, err
		}
		return Outcome{IncidentID: inc.ID, Route: d.Route, Action: "deduped",
			Reason: fmt.Sprintf("occurrence %d", inc.Occurrences)}, nil
	}

	if status == store.StatusHeld {
		if err := e.st.AppendLedger(ctx, inc.ID, now, "system", "held", held); err != nil {
			return Outcome{}, err
		}
		return Outcome{IncidentID: inc.ID, Route: d.Route, Action: "held", Reason: held}, nil
	}

	if err := e.st.AppendLedger(ctx, inc.ID, now, "system", "open",
		fmt.Sprintf("route=%s escalation=%s severity=%s", d.Route, d.Escalation, d.Severity)); err != nil {
		return Outcome{}, err
	}
	// Even the very first page goes through a durable timer, so a crash between
	// "incident created" and "page sent" cannot swallow the page.
	if err := e.st.ScheduleTimer(ctx, inc.ID, 0, now); err != nil {
		return Outcome{}, err
	}
	return Outcome{IncidentID: inc.ID, Route: d.Route, Action: "opened"}, nil
}

// Tick fires every timer that is due. It is safe to call from more than one
// worker and safe to call again after a crash.
func (e *Engine) Tick(ctx context.Context) ([]notify.Page, error) {
	now := e.clk.Now()
	due, err := e.st.DueTimers(ctx, now)
	if err != nil {
		return nil, err
	}
	var sent []notify.Page
	for _, t := range due {
		claimed, err := e.st.ClaimTimer(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		if !claimed {
			continue // another worker owns this timer
		}
		p, fired, err := e.fire(ctx, t, now)
		if err != nil {
			return nil, err
		}
		if fired {
			sent = append(sent, p)
		}
	}
	return sent, nil
}

func (e *Engine) fire(ctx context.Context, t store.Timer, now time.Time) (notify.Page, bool, error) {
	inc, err := e.st.Incident(ctx, t.IncidentID)
	if err != nil {
		return notify.Page{}, false, err
	}
	if inc.Status != store.StatusOpen {
		// Acked or resolved while the timer was pending: drop it silently.
		return notify.Page{}, false, nil
	}
	esc, ok := e.cfg.Escalation(inc.Escalation)
	if !ok {
		return notify.Page{}, false, fmt.Errorf("escalate: incident %d references unknown escalation %q",
			inc.ID, inc.Escalation)
	}
	if t.Step >= len(esc.Steps) {
		return notify.Page{}, false, nil // ladder exhausted
	}
	step := esc.Steps[t.Step]

	page := notify.Page{
		IncidentID: inc.ID, Step: t.Step, Target: e.target(inc.Target),
		Title: inc.Title, Severity: inc.Severity, Fingerprint: inc.Fingerprint,
		At: now, Mention: step.Mention,
	}
	page.Kind = notify.KindPage
	if t.Step > 0 {
		page.Kind = notify.KindEscalate
	}

	kind, arg, _ := strings.Cut(step.Notify, ":")
	responderID := ""
	switch kind {
	case "rota":
		as, err := e.resolver.OnCall(arg, now)
		if err != nil {
			return notify.Page{}, false, err
		}
		r := as.Responder
		page.Responder = &r
		responderID = r.ID
		page.Note = fmt.Sprintf("on call for rota %q (%s)", as.Rota, as.Reason)
	case "responder":
		r, ok := e.cfg.Responder(arg)
		if !ok {
			return notify.Page{}, false, fmt.Errorf("escalate: unknown responder %q", arg)
		}
		page.Responder = &r
		responderID = r.ID
	case "target":
		page.Note = "escalated to the whole group"
	default:
		return notify.Page{}, false, fmt.Errorf("escalate: unsupported notify spec %q", step.Notify)
	}

	first, err := e.st.RecordDelivery(ctx, inc.ID, t.Step, inc.Target, responderID, now)
	if err != nil {
		return notify.Page{}, false, err
	}
	if !first {
		return notify.Page{}, false, nil // already delivered; exactly-once holds
	}

	if err := e.send(ctx, page); err != nil {
		return notify.Page{}, false, err
	}
	detail := fmt.Sprintf("step %d -> %s", t.Step, step.Notify)
	if err := e.st.AppendLedger(ctx, inc.ID, now, "system", string(page.Kind), detail); err != nil {
		return notify.Page{}, false, err
	}
	if err := e.st.SetStep(ctx, inc.ID, t.Step, now); err != nil {
		return notify.Page{}, false, err
	}

	// Arm the next rung, if there is one.
	if t.Step+1 < len(esc.Steps) {
		wait := step.Wait.D()
		if wait <= 0 {
			wait = time.Minute
		}
		if err := e.st.ScheduleTimer(ctx, inc.ID, t.Step+1, now.Add(wait)); err != nil {
			return notify.Page{}, false, err
		}
	}
	return page, true, nil
}

// Ack records an acknowledgement from an inline button press and stops the
// ladder. Acking twice, or acking a resolved incident, is a no-op.
func (e *Engine) Ack(ctx context.Context, incidentID int64, actor string) (bool, error) {
	now := e.clk.Now()
	inc, err := e.st.Incident(ctx, incidentID)
	if err != nil {
		return false, err
	}
	ok, err := e.st.Ack(ctx, incidentID, actor, now)
	if err != nil || !ok {
		return false, err
	}
	if _, err := e.st.CancelTimers(ctx, incidentID); err != nil {
		return false, err
	}
	if err := e.st.AppendLedger(ctx, incidentID, now, actor, "ack",
		fmt.Sprintf("acked at step %d", inc.Step)); err != nil {
		return false, err
	}
	return true, e.send(ctx, notify.Page{
		IncidentID: incidentID, Kind: notify.KindAck, Step: inc.Step,
		Target: e.target(inc.Target), Title: inc.Title, Severity: inc.Severity,
		Fingerprint: inc.Fingerprint, At: now, Note: "acknowledged by " + actor,
	})
}

// Resolve closes an incident by hand.
func (e *Engine) Resolve(ctx context.Context, incidentID int64, actor string) (bool, error) {
	now := e.clk.Now()
	inc, err := e.st.Incident(ctx, incidentID)
	if err != nil {
		return false, err
	}
	ok, err := e.st.Resolve(ctx, incidentID, actor, now)
	if err != nil || !ok {
		return false, err
	}
	if _, err := e.st.CancelTimers(ctx, incidentID); err != nil {
		return false, err
	}
	if err := e.st.AppendLedger(ctx, incidentID, now, actor, "resolve", "manual resolve"); err != nil {
		return false, err
	}
	return true, e.send(ctx, notify.Page{
		IncidentID: incidentID, Kind: notify.KindResolve, Step: inc.Step,
		Target: e.target(inc.Target), Title: inc.Title, Severity: inc.Severity,
		Fingerprint: inc.Fingerprint, At: now, Note: "resolved by " + actor,
	})
}

// Beat records a heartbeat check-in from a cron job.
func (e *Engine) Beat(ctx context.Context, name string) error {
	for _, h := range e.cfg.Heartbeats {
		if h.Name == name {
			return e.st.Beat(ctx, name, e.clk.Now())
		}
	}
	return fmt.Errorf("escalate: unknown heartbeat %q", name)
}

// CheckHeartbeats pages for any dead-man's switch that stopped calling home.
// It pages once per outage: the heartbeat is flagged until the next check-in.
func (e *Engine) CheckHeartbeats(ctx context.Context) ([]Outcome, error) {
	now := e.clk.Now()
	var out []Outcome
	for _, h := range e.cfg.Heartbeats {
		last, seen, err := e.st.LastBeat(ctx, h.Name)
		if err != nil {
			return nil, err
		}
		if !seen {
			continue // never armed; a heartbeat starts counting at its first beat
		}
		deadline := last.Add(h.Interval.D() + h.Grace.D())
		if !now.After(deadline) {
			continue
		}
		fresh, err := e.st.MarkMissed(ctx, h.Name)
		if err != nil {
			return nil, err
		}
		if !fresh {
			continue
		}
		inc, _, err := e.st.CreateIncident(ctx, store.Incident{
			Fingerprint: "heartbeat/" + h.Name,
			Route:       "heartbeat", Escalation: h.Escalation, Target: h.Target,
			Title:    fmt.Sprintf("heartbeat %q missed (last seen %s ago)", h.Name, now.Sub(last).Round(time.Second)),
			Severity: h.Severity, Status: store.StatusOpen,
			Labels:    map[string]string{"alertname": "HeartbeatMissed", "heartbeat": h.Name, "severity": h.Severity},
			CreatedAt: now,
		})
		if err != nil {
			return nil, err
		}
		if err := e.st.AppendLedger(ctx, inc.ID, now, "system", "open",
			"dead-man's switch expired"); err != nil {
			return nil, err
		}
		if err := e.st.ScheduleTimer(ctx, inc.ID, 0, now); err != nil {
			return nil, err
		}
		out = append(out, Outcome{IncidentID: inc.ID, Route: "heartbeat", Action: "opened",
			Reason: "heartbeat " + h.Name + " missed"})
	}
	return out, nil
}

// NextTimer returns when the engine next has work, so a simulator or a runner
// can sleep precisely instead of polling.
func (e *Engine) NextTimer(ctx context.Context) (time.Time, bool, error) {
	t, ok, err := e.st.NextTimer(ctx)
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	return t.FireAt, true, nil
}

func (e *Engine) target(name string) config.Target {
	if t, ok := e.cfg.Target(name); ok {
		return t
	}
	return config.Target{Name: name}
}

func (e *Engine) send(ctx context.Context, p notify.Page) error {
	if e.notifier == nil {
		return errors.New("escalate: no notifier configured")
	}
	return e.notifier.Send(ctx, p)
}
