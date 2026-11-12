package handoff

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/clock"
	"github.com/moveeeax/telegram-oncall/internal/config"
	"github.com/moveeeax/telegram-oncall/internal/escalate"
	"github.com/moveeeax/telegram-oncall/internal/ingest"
	"github.com/moveeeax/telegram-oncall/internal/notify"
	"github.com/moveeeax/telegram-oncall/internal/store"
)

// A daily rota handing over at 09:00 UTC: alice on 2026-03-09, bob on
// 2026-03-10, alice again on 2026-03-11, with carol covering part of the
// 2026-03-11 shift.
const handoffYAML = `
workspace: test
timezone: UTC
targets:
  - name: sre-group
    type: group
    chat_id: "-100"
responders:
  - id: alice
    name: Alice Ng
    telegram_user_id: 1
  - id: bob
    name: Bob Iqbal
    telegram_user_id: 2
  - id: carol
    telegram_user_id: 3
rotas:
  - name: primary
    rotation: daily
    handover_time: "09:00"
    start_date: "2026-03-09"
    members: [alice, bob]
    overrides:
      - responder: carol
        from: 2026-03-11T18:00:00Z
        to: 2026-03-12T06:00:00Z
        reason: alice at the dentist
      - responder: carol
        from: 2026-03-09T12:00:00Z
        to: 2026-03-09T13:00:00Z
escalations:
  - name: standard
    steps:
      - notify: rota:primary
        wait: 5m
      - notify: target
routes:
  - name: all
    match: {}
    target: sre-group
    escalation: standard
dedup:
  flap_window: 30m
  flap_threshold: 3
`

// shiftStart is bob's daily shift: [2026-03-10T09:00Z, 2026-03-11T09:00Z).
var (
	shiftStart = time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	shiftEnd   = shiftStart.Add(24 * time.Hour)
)

type harness struct {
	t   *testing.T
	cfg *config.Config
	st  *store.Store
	clk *clock.Fake
	eng *escalate.Engine
	ctx context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(handoffYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	clk := clock.NewFake(shiftStart)
	// The Recorder stands in for Telegram: nothing here leaves the process.
	return &harness{t: t, cfg: cfg, st: st, clk: clk,
		eng: escalate.New(cfg, st, notify.NewRecorder(), clk), ctx: context.Background()}
}

func alert(name, severity string, status ingest.Status) ingest.Alert {
	labels := map[string]string{"alertname": name, "severity": severity}
	return ingest.Alert{
		Source: "alertmanager", Status: status,
		Fingerprint: ingest.ComputeFingerprint(labels), Labels: labels,
		Title: name + " is firing",
	}
}

// at moves the clock, ingests a, ticks, and returns the incident id.
func (h *harness) fire(at time.Time, a ingest.Alert) int64 {
	h.t.Helper()
	h.clk.Set(at)
	outs, err := h.eng.Ingest(h.ctx, a)
	if err != nil {
		h.t.Fatalf("Ingest: %v", err)
	}
	if _, err := h.eng.Tick(h.ctx); err != nil {
		h.t.Fatalf("Tick: %v", err)
	}
	return outs[0].IncidentID
}

func (h *harness) ack(at time.Time, id int64, who string) {
	h.t.Helper()
	h.clk.Set(at)
	if ok, err := h.eng.Ack(h.ctx, id, who); err != nil || !ok {
		h.t.Fatalf("Ack: ok=%v err=%v", ok, err)
	}
}

func (h *harness) build(opt Options) Report {
	h.t.Helper()
	if opt.Window.Until.IsZero() {
		opt.Window = Window{Since: shiftStart, Until: shiftEnd}
	}
	rep, err := Build(h.ctx, h.st, opt)
	if err != nil {
		h.t.Fatalf("Build: %v", err)
	}
	return rep
}

func TestRepeatFiringsCollapseIntoOneLine(t *testing.T) {
	h := newHarness(t)
	disk := alert("DiskFull", "warning", ingest.StatusFiring)
	first := h.fire(shiftStart.Add(time.Hour), disk)
	h.fire(shiftStart.Add(2*time.Hour), disk) // dedup onto the live incident
	h.fire(shiftStart.Add(3*time.Hour), alert("DiskFull", "warning", ingest.StatusResolved))
	second := h.fire(shiftStart.Add(5*time.Hour), disk) // a fresh incident
	if first == second {
		t.Fatal("a re-firing after resolve should open a new incident")
	}
	h.fire(shiftStart.Add(6*time.Hour), alert("Down", "critical", ingest.StatusFiring))

	rep := h.build(Options{})
	if len(rep.Fired) != 2 {
		t.Fatalf("fired groups = %+v, want 2", rep.Fired)
	}
	// Critical sorts before warning regardless of firing counts.
	if rep.Fired[0].Title != "Down is firing" {
		t.Errorf("first group = %+v, want the critical one", rep.Fired[0])
	}
	g := rep.Fired[1]
	if g.Incidents != 2 || g.Firings != 3 || g.LastStatus != store.StatusOpen {
		t.Errorf("disk group = %+v, want 2 incidents, 3 firings, open", g)
	}
	want := Counts{Opened: 3, Deduped: 1, Pages: 3, Escalations: 2, Resolves: 1}
	if rep.Counts != want {
		t.Errorf("counts = %+v, want %+v", rep.Counts, want)
	}
}

func TestLiveIncidentsAreCarriedForward(t *testing.T) {
	h := newHarness(t)
	// Fired during the previous shift, never resolved: still the next person's
	// problem even though nothing new happened to it in this window.
	old := h.fire(shiftStart.Add(-3*time.Hour), alert("CertExpiry", "warning", ingest.StatusFiring))
	acked := h.fire(shiftStart.Add(time.Hour), alert("Down", "critical", ingest.StatusFiring))
	h.ack(shiftStart.Add(time.Hour+4*time.Minute), acked, "bob")
	// Acked only after the handover: at the handover it was not acked.
	late := h.fire(shiftEnd.Add(-10*time.Minute), alert("Latency", "critical", ingest.StatusFiring))
	h.ack(shiftEnd.Add(10*time.Minute), late, "alice")
	// Fired after the window: belongs to the next handoff.
	h.fire(shiftEnd.Add(time.Minute), alert("Later", "critical", ingest.StatusFiring))

	rep := h.build(Options{})
	if len(rep.Live) != 3 {
		t.Fatalf("live = %+v, want 3", rep.Live)
	}
	if rep.Live[0].ID != old || rep.Live[0].Status != store.StatusOpen {
		t.Errorf("live[0] = %+v, want the incident from the previous shift", rep.Live[0])
	}
	if rep.Live[1].ID != acked || rep.Live[1].Status != store.StatusAcked || rep.Live[1].AckedBy != "bob" {
		t.Errorf("live[1] = %+v, want acked by bob", rep.Live[1])
	}
	if rep.Live[2].ID != late || rep.Live[2].Status != store.StatusOpen || rep.Live[2].AckedBy != "" {
		t.Errorf("live[2] = %+v, want open at the handover", rep.Live[2])
	}
	for _, g := range rep.Fired {
		if g.Title == "CertExpiry is firing" || g.Title == "Later is firing" {
			t.Errorf("%q fired outside the window but is listed as fired in it", g.Title)
		}
	}
}

func TestHeldIncidentsShowWhyTheyWereQuiet(t *testing.T) {
	h := newHarness(t)
	flappy := alert("Flappy", "warning", ingest.StatusFiring)
	resolved := alert("Flappy", "warning", ingest.StatusResolved)
	for i := 0; i < 4; i++ {
		at := shiftStart.Add(time.Duration(i) * 2 * time.Minute)
		h.fire(at, flappy)
		h.fire(at.Add(time.Minute), resolved)
	}
	h.fire(shiftStart.Add(20*time.Minute), flappy)

	rep := h.build(Options{})
	if len(rep.Fired) != 1 || !rep.Fired[0].Flapping || rep.Fired[0].Firings != 5 || rep.Fired[0].Incidents != 5 {
		t.Fatalf("fired = %+v, want one flapping group with 5 incidents", rep.Fired)
	}
	if len(rep.Live) != 1 || rep.Live[0].Status != store.StatusHeld ||
		!strings.Contains(rep.Live[0].HeldReason, "flapping") {
		t.Fatalf("live = %+v, want one held flapping incident", rep.Live)
	}
}

func TestShiftWindowAtTheHandoverIsTheEndingShift(t *testing.T) {
	h := newHarness(t)
	w, err := ShiftWindow(h.cfg, "primary", shiftEnd)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Since.Equal(shiftStart) || !w.Until.Equal(shiftEnd) {
		t.Fatalf("window = %s..%s, want %s..%s", w.Since, w.Until, shiftStart, shiftEnd)
	}
	// Mid-shift, the window starts at the same handover.
	w, err = ShiftWindow(h.cfg, "primary", shiftStart.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Since.Equal(shiftStart) {
		t.Fatalf("mid-shift window starts at %s, want %s", w.Since, shiftStart)
	}
	if _, err := ShiftWindow(h.cfg, "nope", shiftEnd); err == nil {
		t.Fatal("expected an error for an unknown rota")
	}
}

func TestRotaAndOverridesForTheIncomingShift(t *testing.T) {
	h := newHarness(t)
	rep := h.build(Options{Config: h.cfg, Rota: "primary"})
	if len(rep.Shifts) != 1 {
		t.Fatalf("shifts = %+v", rep.Shifts)
	}
	s := rep.Shifts[0]
	if s.Outgoing.ID != "bob" || s.Incoming.ID != "alice" || !s.IncomingUntil.Equal(shiftEnd.Add(24*time.Hour)) {
		t.Errorf("shift = %+v, want bob -> alice until %s", s, shiftEnd.Add(24*time.Hour))
	}
	// Only the override that lands inside alice's incoming shift is listed;
	// the one from 2026-03-09 is history.
	if len(rep.Overrides) != 1 || rep.Overrides[0].Responder != "carol" ||
		rep.Overrides[0].Reason != "alice at the dentist" {
		t.Fatalf("overrides = %+v, want carol covering alice", rep.Overrides)
	}
	if rep.Workspace != "test" {
		t.Errorf("workspace = %q", rep.Workspace)
	}
}

func TestBuildRejectsAnEmptyWindow(t *testing.T) {
	h := newHarness(t)
	if _, err := Build(h.ctx, h.st, Options{Window: Window{Since: shiftEnd, Until: shiftEnd}}); err == nil {
		t.Fatal("expected an error for since == until")
	}
	if _, err := Build(h.ctx, h.st, Options{Window: Window{Since: shiftEnd, Until: shiftStart}}); err == nil {
		t.Fatal("expected an error for since > until")
	}
}

func render(t *testing.T, fn func(*bytes.Buffer, Report) error, r Report) string {
	t.Helper()
	var b bytes.Buffer
	if err := fn(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func markdown(b *bytes.Buffer, r Report) error { return Markdown(b, r) }
func short(b *bytes.Buffer, r Report) error    { return Short(b, r) }

func TestMarkdownIsStableAndComplete(t *testing.T) {
	h := newHarness(t)
	id := h.fire(shiftStart.Add(time.Hour), alert("Down", "critical", ingest.StatusFiring))
	h.ack(shiftStart.Add(time.Hour+time.Minute), id, "bob")
	pipe := alert("Pipe", "warning", ingest.StatusFiring)
	pipe.Title = "a | b"
	h.fire(shiftStart.Add(2*time.Hour), pipe)

	opt := Options{Config: h.cfg, Rota: "primary"}
	first := render(t, markdown, h.build(opt))
	second := render(t, markdown, h.build(opt))
	if first != second {
		t.Fatalf("two builds over the same window differ:\n%s\n---\n%s", first, second)
	}
	for _, want := range []string{
		"# On-call handoff: test\n",
		"Window: 2026-03-10T09:00:00Z to 2026-03-11T09:00:00Z (24h0m0s)",
		"- primary: bob (Bob Iqbal) hands over to alice (Alice Ng), on call until 2026-03-12T09:00:00Z",
		"2 opened, 0 held, 0 deduped, 2 page(s), 0 escalation(s), 1 ack(s), 0 resolve(s)",
		"| CRITICAL | Down is firing | all | 1 | 1 | acked |",
		`| WARNING | a \| b | all | 1 | 1 | open |`,
		"- #1 CRITICAL Down is firing (route all): acked by bob, open since 2026-03-10T10:00:00Z",
		"- #2 WARNING a | b (route all): NOT ACKED, open since 2026-03-10T11:00:00Z",
		"- primary: carol covers 2026-03-11T18:00:00Z to 2026-03-12T06:00:00Z (alice at the dentist)",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("missing %q in:\n%s", want, first)
		}
	}
}

func TestMarkdownWithoutConfigOrIncidents(t *testing.T) {
	h := newHarness(t)
	out := render(t, markdown, h.build(Options{}))
	for _, want := range []string{"# On-call handoff\n", "Nothing fired.", "Nothing is open."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "## Rota") || strings.Contains(out, "## Overrides") {
		t.Errorf("rota sections need a config:\n%s", out)
	}
}

func TestShortFormCapsItsLists(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < shortMaxLive+2; i++ {
		h.fire(shiftStart.Add(time.Duration(i)*time.Minute),
			alert(fmt.Sprintf("Alert%02d", i), "critical", ingest.StatusFiring))
	}
	noisy := alert("Noisy", "info", ingest.StatusFiring)
	noisy.Title = strings.Repeat("x", 300)
	for i := 0; i < 3; i++ {
		h.fire(shiftStart.Add(time.Hour+time.Duration(i)*time.Minute), noisy)
	}

	out := render(t, short, h.build(Options{Config: h.cfg, Rota: "primary"}))
	for _, want := range []string{
		"Handoff test 2026-03-10T09:00:00Z to 2026-03-11T09:00:00Z\n",
		"primary: bob -> alice until 2026-03-12T09:00:00Z\n",
		"Fired: 13 alert(s), 15 firing(s). Still live: 13. Acks: 0.\n",
		"  +3 more\n", // 13 live, 10 shown
		"  +8 more\n", // 13 fired groups, 5 shown
		"  3x INFO " + strings.Repeat("x", shortMaxTitle-3) + "... (open)\n",
		"Override primary: carol 2026-03-11T18:00:00Z to 2026-03-12T06:00:00Z\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "\n  #"); n != shortMaxLive {
		t.Errorf("short form lists %d live incidents, want %d", n, shortMaxLive)
	}
}
