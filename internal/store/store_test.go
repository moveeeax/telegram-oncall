package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 3, 13, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newIncident(t *testing.T, s *Store, fp, route string) Incident {
	t.Helper()
	inc, dedup, err := s.CreateIncident(context.Background(), Incident{
		Fingerprint: fp, Route: route, Escalation: "std", Target: "g",
		Title: "t", Severity: "critical", Status: StatusOpen,
		Labels: map[string]string{"alertname": "X"}, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if dedup {
		t.Fatal("unexpected dedup on a fresh fingerprint")
	}
	return inc
}

// Two in-memory stores in one process must not share state, or every test
// contaminates the next.
func TestInMemoryStoresAreIsolated(t *testing.T) {
	a, b := open(t), open(t)
	newIncident(t, a, "fp", "r")
	list, err := b.ListIncidents(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("second store saw %d incidents from the first", len(list))
	}
}

func TestLiveIncidentUniquenessAndRecycling(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := newIncident(t, s, "fp", "r")

	// Same fingerprint + route while live: dedup, not a new row.
	again, dedup, err := s.CreateIncident(ctx, Incident{
		Fingerprint: "fp", Route: "r", Escalation: "std", Target: "g",
		Title: "t", Severity: "critical", Status: StatusOpen, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dedup || again.ID != first.ID || again.Occurrences != 2 {
		t.Fatalf("dedup=%v id=%d occurrences=%d", dedup, again.ID, again.Occurrences)
	}

	// A different route for the same fingerprint is a separate incident.
	other := newIncident(t, s, "fp", "audit")
	if other.ID == first.ID {
		t.Fatal("a second route reused the first incident")
	}

	// Once resolved, the fingerprint is free again.
	if ok, err := s.Resolve(ctx, first.ID, "alice", now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	fresh := newIncident(t, s, "fp", "r")
	if fresh.ID == first.ID {
		t.Fatal("a re-firing after resolve should open a new incident")
	}
}

func TestClaimTimerIsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	inc := newIncident(t, s, "fp", "r")

	if err := s.ScheduleTimer(ctx, inc.ID, 0, now); err != nil {
		t.Fatal(err)
	}
	// Re-scheduling the same step is a no-op, not a duplicate timer.
	if err := s.ScheduleTimer(ctx, inc.ID, 0, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueTimers(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d due timers, want 1", len(due))
	}

	ok, err := s.ClaimTimer(ctx, due[0].ID)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	ok, err = s.ClaimTimer(ctx, due[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("the same timer was claimed twice")
	}
	if due, _ := s.DueTimers(ctx, now); len(due) != 0 {
		t.Fatalf("a fired timer is still due: %+v", due)
	}
}

func TestRecordDeliveryIsExactlyOncePerStep(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	inc := newIncident(t, s, "fp", "r")

	first, err := s.RecordDelivery(ctx, inc.ID, 0, "g", "alice", now)
	if err != nil || !first {
		t.Fatalf("first delivery: %v %v", first, err)
	}
	second, err := s.RecordDelivery(ctx, inc.ID, 0, "g", "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("step 0 was recorded as delivered twice")
	}
	next, err := s.RecordDelivery(ctx, inc.ID, 1, "g", "bob", now)
	if err != nil || !next {
		t.Fatalf("step 1 delivery: %v %v", next, err)
	}
}

func TestCancelTimersAndAckTransitions(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	inc := newIncident(t, s, "fp", "r")
	for step := 0; step < 3; step++ {
		if err := s.ScheduleTimer(ctx, inc.ID, step, now.Add(time.Duration(step)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.CancelTimers(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("cancelled %d timers, want 3", n)
	}
	if _, ok, err := s.NextTimer(ctx); err != nil || ok {
		t.Fatalf("a pending timer survived cancellation (ok=%v err=%v)", ok, err)
	}

	if ok, _ := s.Ack(ctx, inc.ID, "alice", now); !ok {
		t.Fatal("first ack rejected")
	}
	if ok, _ := s.Ack(ctx, inc.ID, "bob", now); ok {
		t.Fatal("second ack accepted")
	}
	got, err := s.Incident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAcked || got.AckedBy != "alice" || got.AckedAt == nil {
		t.Fatalf("incident after ack = %+v", got)
	}
	if got.Labels["alertname"] != "X" {
		t.Errorf("labels did not round-trip: %+v", got.Labels)
	}
}

func TestFlapCountingIsWindowed(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	window := 30 * time.Minute

	if n, _ := s.RecordFiring(ctx, "fp", now, window); n != 1 {
		t.Fatalf("first firing counted %d", n)
	}
	if n, _ := s.RecordFiring(ctx, "fp", now.Add(time.Minute), window); n != 2 {
		t.Fatalf("second firing counted %d", n)
	}
	// An hour later the earlier firings have aged out of the window.
	if n, _ := s.RecordFiring(ctx, "fp", now.Add(time.Hour), window); n != 1 {
		t.Fatalf("firing after the window counted %d, want 1", n)
	}
	// Other fingerprints are counted separately.
	if n, _ := s.RecordFiring(ctx, "other", now.Add(time.Hour), window); n != 1 {
		t.Fatalf("unrelated fingerprint counted %d", n)
	}
}

func TestHeartbeatBeatResetsMissedFlag(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, seen, err := s.LastBeat(ctx, "hb"); err != nil || seen {
		t.Fatalf("unseen heartbeat reported as seen (err=%v)", err)
	}
	if err := s.Beat(ctx, "hb", now); err != nil {
		t.Fatal(err)
	}
	last, seen, err := s.LastBeat(ctx, "hb")
	if err != nil || !seen || !last.Equal(now) {
		t.Fatalf("LastBeat = %s seen=%v err=%v", last, seen, err)
	}
	if ok, _ := s.MarkMissed(ctx, "hb"); !ok {
		t.Fatal("first MarkMissed should win")
	}
	if ok, _ := s.MarkMissed(ctx, "hb"); ok {
		t.Fatal("MarkMissed fired twice for one outage")
	}
	// A fresh beat re-arms it.
	if err := s.Beat(ctx, "hb", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.MarkMissed(ctx, "hb"); !ok {
		t.Fatal("MarkMissed should be armed again after a check-in")
	}
}

func TestIncidentNotFound(t *testing.T) {
	if _, err := open(t).Incident(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestIncidentsLiveAtIsHalfOpen(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	mk := func(fp string, at time.Time) Incident {
		inc, _, err := s.CreateIncident(ctx, Incident{
			Fingerprint: fp, Route: "r", Escalation: "std", Target: "g",
			Title: fp, Severity: "critical", Status: StatusOpen, CreatedAt: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		return inc
	}
	cut := now.Add(time.Hour)
	resolvedBefore := mk("resolved-before", now)
	resolvedAtCut := mk("resolved-at-cut", now)
	stillOpen := mk("still-open", now.Add(time.Minute))
	mk("created-at-cut", cut)
	if _, err := s.Resolve(ctx, resolvedBefore.ID, "alice", cut.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, resolvedAtCut.ID, "alice", cut); err != nil {
		t.Fatal(err)
	}

	got, err := s.IncidentsLiveAt(ctx, cut)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, inc := range got {
		ids = append(ids, inc.ID)
	}
	if len(ids) != 2 || ids[0] != resolvedAtCut.ID || ids[1] != stillOpen.ID {
		t.Fatalf("live at cut = %v, want [%d %d]", ids, resolvedAtCut.ID, stillOpen.ID)
	}
}

func TestLedgerBetweenIsHalfOpen(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	inc := newIncident(t, s, "fp", "r")
	for i, action := range []string{"open", "page", "ack", "resolve"} {
		if err := s.AppendLedger(ctx, inc.ID, now.Add(time.Duration(i)*time.Minute), "system", action, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LedgerBetween(ctx, now.Add(time.Minute), now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "page" || got[1].Action != "ack" {
		t.Fatalf("ledger between = %+v, want page and ack", got)
	}
}
