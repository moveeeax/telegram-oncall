package escalate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/clock"
	"github.com/moveeeax/telegram-oncall/internal/config"
	"github.com/moveeeax/telegram-oncall/internal/ingest"
	"github.com/moveeeax/telegram-oncall/internal/notify"
	"github.com/moveeeax/telegram-oncall/internal/store"
)

const engineYAML = `
workspace: test
timezone: UTC
targets:
  - name: sre-group
    type: group
    chat_id: "-100"
responders:
  - id: alice
    telegram_user_id: 1
  - id: bob
    telegram_user_id: 2
rotas:
  - name: primary
    rotation: weekly
    handover_time: "10:00"
    start_date: "2026-01-05"
    members: [alice]
  - name: secondary
    rotation: weekly
    handover_time: "10:00"
    start_date: "2026-01-05"
    members: [bob]
escalations:
  - name: standard
    steps:
      - notify: rota:primary
        wait: 5m
      - notify: rota:secondary
        wait: 5m
      - notify: target
        mention: true
routes:
  - name: all
    match: {}
    target: sre-group
    escalation: standard
quiet_hours:
  from: "23:00"
  to: "08:00"
  suppress: [warning]
  breakthrough: [critical]
heartbeats:
  - name: nightly-backup
    interval: 1h
    grace: 10m
    target: sre-group
    escalation: standard
    severity: critical
dedup:
  flap_window: 30m
  flap_threshold: 3
`

var t0 = time.Date(2026, 3, 13, 12, 0, 0, 0, time.UTC)

type harness struct {
	cfg *config.Config
	st  *store.Store
	rec *notify.Recorder
	clk *clock.Fake
	eng *Engine
	ctx context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(engineYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	rec := notify.NewRecorder()
	clk := clock.NewFake(t0)
	return &harness{cfg: cfg, st: st, rec: rec, clk: clk,
		eng: New(cfg, st, rec, clk), ctx: context.Background()}
}

func firing(severity string, extra ...string) ingest.Alert {
	labels := map[string]string{"alertname": "DiskFull", "severity": severity}
	for i := 0; i+1 < len(extra); i += 2 {
		labels[extra[i]] = extra[i+1]
	}
	return ingest.Alert{
		Source: "alertmanager", Status: ingest.StatusFiring,
		Fingerprint: ingest.ComputeFingerprint(labels), Labels: labels,
		Title: "Disk full on /var", StartsAt: t0,
	}
}

func (h *harness) ingest(t *testing.T, a ingest.Alert) []Outcome {
	t.Helper()
	out, err := h.eng.Ingest(h.ctx, a)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return out
}

func (h *harness) tick(t *testing.T) []notify.Page {
	t.Helper()
	p, err := h.eng.Tick(h.ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	return p
}

func TestLadderClimbsUntilExhausted(t *testing.T) {
	h := newHarness(t)
	outs := h.ingest(t, firing("critical"))
	if len(outs) != 1 || outs[0].Action != "opened" {
		t.Fatalf("ingest outcome = %+v", outs)
	}
	id := outs[0].IncidentID

	// Step 0 is armed for "now", so it needs a tick, not a wait.
	pages := h.tick(t)
	if len(pages) != 1 || pages[0].Step != 0 || pages[0].Responder.ID != "alice" {
		t.Fatalf("first page = %+v", pages)
	}
	if pages[0].Kind != notify.KindPage {
		t.Errorf("first page kind = %q, want page", pages[0].Kind)
	}
	if got := pages[0].Buttons(); len(got) != 2 {
		t.Errorf("page must carry ack/resolve buttons, got %v", got)
	}

	// Nothing more is due before the 5 minute wait elapses.
	h.clk.Advance(4 * time.Minute)
	if p := h.tick(t); len(p) != 0 {
		t.Fatalf("escalated early: %+v", p)
	}

	h.clk.Advance(time.Minute)
	pages = h.tick(t)
	if len(pages) != 1 || pages[0].Step != 1 || pages[0].Responder.ID != "bob" {
		t.Fatalf("second page = %+v", pages)
	}
	if pages[0].Kind != notify.KindEscalate {
		t.Errorf("second page kind = %q, want escalate", pages[0].Kind)
	}

	h.clk.Advance(5 * time.Minute)
	pages = h.tick(t)
	if len(pages) != 1 || pages[0].Step != 2 || !pages[0].Mention {
		t.Fatalf("third page = %+v", pages)
	}
	if pages[0].Responder != nil {
		t.Errorf("the group step should not name a responder, got %+v", pages[0].Responder)
	}

	// Ladder exhausted: no further timers, no further pages.
	h.clk.Advance(time.Hour)
	if p := h.tick(t); len(p) != 0 {
		t.Fatalf("paged past the end of the ladder: %+v", p)
	}
	if _, ok, err := h.eng.NextTimer(h.ctx); err != nil || ok {
		t.Fatalf("timer left pending after the ladder ended (ok=%v err=%v)", ok, err)
	}
	inc, err := h.st.Incident(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.Status != store.StatusOpen || inc.Step != 2 {
		t.Errorf("incident = status %q step %d, want open/2", inc.Status, inc.Step)
	}
}

func TestAckStopsTheLadder(t *testing.T) {
	h := newHarness(t)
	id := h.ingest(t, firing("critical"))[0].IncidentID
	h.tick(t)

	h.clk.Advance(2 * time.Minute)
	ok, err := h.eng.Ack(h.ctx, id, "alice")
	if err != nil || !ok {
		t.Fatalf("Ack: ok=%v err=%v", ok, err)
	}

	h.clk.Advance(time.Hour)
	if p := h.tick(t); len(p) != 0 {
		t.Fatalf("escalated after ack: %+v", p)
	}

	inc, err := h.st.Incident(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inc.Status != store.StatusAcked || inc.AckedBy != "alice" {
		t.Fatalf("incident = %q by %q, want acked by alice", inc.Status, inc.AckedBy)
	}
	if inc.AckedAt == nil || inc.AckedAt.Sub(inc.CreatedAt) != 2*time.Minute {
		t.Errorf("ack latency not recorded correctly: created %s acked %v", inc.CreatedAt, inc.AckedAt)
	}

	// A second ack is a no-op, not an error and not a second ledger row.
	ok, err = h.eng.Ack(h.ctx, id, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("double ack was accepted")
	}
	entries, err := h.st.Ledger(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	acks := 0
	for _, e := range entries {
		if e.Action == "ack" {
			acks++
		}
	}
	if acks != 1 {
		t.Errorf("ledger has %d ack rows, want 1", acks)
	}
}

// A crash between "incident opened" and "page delivered" must not lose the
// page, and a crash mid-ladder must not lose the escalation: all state is in
// the database, so a fresh Engine over the same store carries on.
func TestRestartSafety(t *testing.T) {
	h := newHarness(t)
	id := h.ingest(t, firing("critical"))[0].IncidentID

	// Process dies here — before the very first tick.
	rec2 := notify.NewRecorder()
	clk2 := clock.NewFake(t0.Add(7 * time.Minute)) // it was down for 7 minutes
	eng2 := New(h.cfg, h.st, rec2, clk2)

	pages, err := eng2.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].Step != 0 {
		t.Fatalf("the first page was lost across the restart: %+v", pages)
	}
	if pages[0].IncidentID != id {
		t.Errorf("paged incident %d, want %d", pages[0].IncidentID, id)
	}
	// The next rung is armed relative to when the page actually went out.
	next, ok, err := eng2.NextTimer(context.Background())
	if err != nil || !ok {
		t.Fatalf("no timer armed after restart (ok=%v err=%v)", ok, err)
	}
	if want := t0.Add(12 * time.Minute); !next.Equal(want) {
		t.Errorf("next timer at %s, want %s", next, want)
	}
}

// Two workers ticking the same database must not double-page: the unique index
// on (incident_id, step) plus the atomic timer claim is what guarantees it.
func TestExactlyOnceUnderConcurrentTicks(t *testing.T) {
	h := newHarness(t)
	h.ingest(t, firing("critical"))

	recs := []*notify.Recorder{notify.NewRecorder(), notify.NewRecorder(), notify.NewRecorder()}
	var wg sync.WaitGroup
	for _, rec := range recs {
		wg.Add(1)
		go func(r *notify.Recorder) {
			defer wg.Done()
			e := New(h.cfg, h.st, r, h.clk)
			if _, err := e.Tick(context.Background()); err != nil {
				t.Errorf("Tick: %v", err)
			}
		}(rec)
	}
	wg.Wait()

	total := 0
	for _, r := range recs {
		total += len(r.Pages())
	}
	if total != 1 {
		t.Fatalf("%d workers delivered %d pages, want exactly 1", len(recs), total)
	}
}

func TestDedupCollapsesRepeatFirings(t *testing.T) {
	h := newHarness(t)
	a := firing("critical")
	first := h.ingest(t, a)[0]
	h.tick(t)

	for i := 0; i < 2; i++ {
		h.clk.Advance(time.Minute)
		out := h.ingest(t, a)[0]
		if out.Action != "deduped" {
			t.Fatalf("repeat firing %d: action = %q, want deduped", i, out.Action)
		}
		if out.IncidentID != first.IncidentID {
			t.Fatalf("repeat firing opened a second incident %d", out.IncidentID)
		}
	}
	if p := h.rec.Pages(); len(p) != 1 {
		t.Fatalf("dedup still paged %d times, want 1", len(p))
	}
	inc, err := h.st.Incident(h.ctx, first.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if inc.Occurrences != 3 {
		t.Errorf("occurrences = %d, want 3", inc.Occurrences)
	}
}

func TestFlapSuppression(t *testing.T) {
	h := newHarness(t)
	a := firing("critical")
	// flap_threshold is 3, so the fourth firing inside the window is held.
	for i := 0; i < 3; i++ {
		out := h.ingest(t, a)[0]
		if i == 0 && out.Action != "opened" {
			t.Fatalf("first firing action = %q", out.Action)
		}
		h.clk.Advance(time.Minute)
		if _, err := h.eng.Resolve(h.ctx, out.IncidentID, "source"); err != nil {
			t.Fatal(err)
		}
		h.clk.Advance(time.Minute)
	}
	out := h.ingest(t, a)[0]
	if out.Action != "held" {
		t.Fatalf("fourth firing action = %q, want held (%s)", out.Action, out.Reason)
	}
	inc, err := h.st.Incident(h.ctx, out.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if !inc.Flapping || inc.Status != store.StatusHeld {
		t.Errorf("incident = flapping %v status %q", inc.Flapping, inc.Status)
	}
	// Held incidents arm no timers, so no page ever leaves.
	h.clk.Advance(time.Hour)
	if p := h.tick(t); len(p) != 0 {
		t.Fatalf("a flapping incident paged: %+v", p)
	}
}

func TestQuietHoursHoldsButStillRecords(t *testing.T) {
	h := newHarness(t)
	h.clk.Set(time.Date(2026, 3, 14, 2, 0, 0, 0, time.UTC)) // 02:00 UTC, inside 23:00-08:00

	out := h.ingest(t, firing("warning"))[0]
	if out.Action != "held" {
		t.Fatalf("warning at 02:00 action = %q, want held", out.Action)
	}
	h.clk.Advance(time.Hour)
	if p := h.tick(t); len(p) != 0 {
		t.Fatalf("held incident paged: %+v", p)
	}
	entries, err := h.st.Ledger(h.ctx, out.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != "held" {
		t.Fatalf("ledger = %+v, want one held row", entries)
	}

	// Critical breaks through at the same hour.
	crit := h.ingest(t, firing("critical", "alertname", "OtherThing"))[0]
	if crit.Action != "opened" {
		t.Fatalf("critical at 03:00 action = %q, want opened", crit.Action)
	}
	if p := h.tick(t); len(p) != 1 {
		t.Fatalf("critical did not break through quiet hours: %+v", p)
	}
}

func TestResolvedAlertClosesIncidentAndCancelsTimers(t *testing.T) {
	h := newHarness(t)
	a := firing("critical")
	id := h.ingest(t, a)[0].IncidentID
	h.tick(t)

	h.clk.Advance(time.Minute)
	resolved := a
	resolved.Status = ingest.StatusResolved
	out := h.ingest(t, resolved)[0]
	if out.Action != "resolved" || out.IncidentID != id {
		t.Fatalf("resolve outcome = %+v", out)
	}
	if _, ok, err := h.eng.NextTimer(h.ctx); err != nil || ok {
		t.Fatal("timers survived resolution")
	}
	h.clk.Advance(time.Hour)
	if p := h.tick(t); len(p) != 0 {
		t.Fatalf("escalated after resolve: %+v", p)
	}

	// The same fingerprint can open a fresh incident afterwards.
	h.clk.Advance(time.Hour)
	again := h.ingest(t, a)[0]
	if again.Action != "opened" || again.IncidentID == id {
		t.Fatalf("re-firing after resolve = %+v, want a new incident", again)
	}
}

func TestUnroutedAlertIsReportedNotSwallowed(t *testing.T) {
	cfg, err := config.Parse([]byte(`
workspace: t
targets:
  - name: g
    type: group
    chat_id: "-1"
responders:
  - id: a
    telegram_user_id: 1
escalations:
  - name: e
    steps:
      - notify: target
routes:
  - name: only-db
    match:
      labels:
        team: db
    target: g
    escalation: e
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng := New(cfg, st, notify.NewRecorder(), clock.NewFake(t0))
	outs, err := eng.Ingest(context.Background(), firing("critical"))
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].Action != "unrouted" {
		t.Fatalf("outcome = %+v, want unrouted", outs)
	}
}

func TestHeartbeatPagesOncePerOutage(t *testing.T) {
	h := newHarness(t)
	if err := h.eng.Beat(h.ctx, "nightly-backup"); err != nil {
		t.Fatal(err)
	}
	// Well inside interval+grace: silence.
	h.clk.Advance(time.Hour)
	outs, err := h.eng.CheckHeartbeats(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 0 {
		t.Fatalf("paged inside the grace window: %+v", outs)
	}

	h.clk.Advance(11 * time.Minute) // 1h11m > 1h + 10m
	outs, err = h.eng.CheckHeartbeats(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].Action != "opened" {
		t.Fatalf("dead-man's switch did not fire: %+v", outs)
	}
	pages := h.tick(t)
	if len(pages) != 1 || pages[0].Responder.ID != "alice" {
		t.Fatalf("heartbeat page = %+v", pages)
	}

	// Checking again must not open a second incident for the same outage.
	h.clk.Advance(time.Hour)
	outs, err = h.eng.CheckHeartbeats(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 0 {
		t.Fatalf("heartbeat paged twice for one outage: %+v", outs)
	}

	if err := h.eng.Beat(h.ctx, "unknown"); err == nil {
		t.Error("beating an undeclared heartbeat should error")
	}
}

func TestLedgerRecordsActorAndTimestamps(t *testing.T) {
	h := newHarness(t)
	id := h.ingest(t, firing("critical"))[0].IncidentID
	h.tick(t)
	h.clk.Advance(5 * time.Minute)
	h.tick(t)
	h.clk.Advance(time.Minute)
	if _, err := h.eng.Ack(h.ctx, id, "bob"); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(time.Minute)
	if _, err := h.eng.Resolve(h.ctx, id, "bob"); err != nil {
		t.Fatal(err)
	}

	entries, err := h.st.Ledger(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"open", "page", "escalate", "ack", "resolve"}
	if len(entries) != len(want) {
		t.Fatalf("ledger has %d rows, want %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		if entries[i].Action != w {
			t.Errorf("row %d action = %q, want %q", i, entries[i].Action, w)
		}
		if i > 0 && entries[i].At.Before(entries[i-1].At) {
			t.Errorf("row %d is out of chronological order", i)
		}
	}
	if entries[3].Actor != "bob" {
		t.Errorf("ack actor = %q, want bob", entries[3].Actor)
	}
}
