package rota

import (
	"testing"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/config"
)

const rotaYAML = `
workspace: test
timezone: Europe/Amsterdam
targets:
  - name: g
    type: group
    chat_id: "-100"
responders:
  - id: alice
    telegram_user_id: 1
  - id: bob
    telegram_user_id: 2
  - id: carol
    telegram_user_id: 3
rotas:
  - name: weekly
    rotation: weekly
    handover_time: "10:00"
    start_date: "2026-01-05"
    members: [alice, bob, carol]
  - name: daily
    rotation: daily
    handover_time: "09:00"
    start_date: "2026-01-05"
    members: [alice, bob]
  - name: covered
    rotation: weekly
    handover_time: "10:00"
    start_date: "2026-01-05"
    members: [alice, bob, carol]
    overrides:
      - responder: carol
        from: 2026-01-06T18:00:00Z
        to: 2026-01-07T08:00:00Z
        reason: alice at a wedding
escalations:
  - name: e
    steps:
      - notify: target
routes:
  - name: all
    target: g
    escalation: e
`

func load(t *testing.T) *Resolver {
	t.Helper()
	cfg, err := config.Parse([]byte(rotaYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return NewResolver(cfg)
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return v.UTC()
}

func TestWeeklyRotationAdvancesAtHandover(t *testing.T) {
	r := load(t)
	// Handover is 10:00 Europe/Amsterdam = 09:00Z in winter.
	cases := []struct {
		when string
		want string
	}{
		{"2026-01-05T08:59:00Z", "carol"}, // before the very first handover: previous period
		{"2026-01-05T09:00:00Z", "alice"}, // first period starts
		{"2026-01-11T23:59:00Z", "alice"}, // still week 1
		{"2026-01-12T08:59:00Z", "alice"}, // minute before handover
		{"2026-01-12T09:00:00Z", "bob"},   // week 2
		{"2026-01-19T09:00:00Z", "carol"}, // week 3
		{"2026-01-26T09:00:00Z", "alice"}, // wraps back round
	}
	for _, c := range cases {
		a, err := r.OnCall("weekly", at(t, c.when))
		if err != nil {
			t.Fatalf("%s: %v", c.when, err)
		}
		if a.Responder.ID != c.want {
			t.Errorf("at %s on call = %s, want %s (%s)", c.when, a.Responder.ID, c.want, a.Reason)
		}
	}
}

func TestDailyRotation(t *testing.T) {
	r := load(t)
	cases := map[string]string{
		"2026-01-05T07:59:00Z": "bob",   // 08:59 local, before the first handover
		"2026-01-05T08:00:00Z": "alice", // 09:00 local, handover exactly
		"2026-01-06T08:01:00Z": "bob",
		"2026-01-07T08:01:00Z": "alice",
	}
	for when, want := range cases {
		a, err := r.OnCall("daily", at(t, when))
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if a.Responder.ID != want {
			t.Errorf("at %s on call = %s, want %s", when, a.Responder.ID, want)
		}
	}
}

// A rotation computed as "start + N*168h" drifts by an hour across a DST
// transition and hands over at the wrong local time. Handover must stay at
// 10:00 local on both sides of the change.
func TestHandoverIsDSTStableInLocalTime(t *testing.T) {
	r := load(t)
	// Europe/Amsterdam moves to summer time on 2026-03-29, so 10:00 local is
	// 08:00Z from 2026-03-30 onwards but 09:00Z before it.
	before, err := r.OnCall("weekly", at(t, "2026-03-23T09:00:00Z")) // 10:00 local, winter
	if err != nil {
		t.Fatal(err)
	}
	if !before.PeriodStart.Equal(at(t, "2026-03-23T09:00:00Z")) {
		t.Errorf("winter period start = %s, want 2026-03-23T09:00:00Z", before.PeriodStart)
	}
	after, err := r.OnCall("weekly", at(t, "2026-03-30T08:00:00Z")) // 10:00 local, summer
	if err != nil {
		t.Fatal(err)
	}
	if !after.PeriodStart.Equal(at(t, "2026-03-30T08:00:00Z")) {
		t.Errorf("summer period start = %s, want 2026-03-30T08:00:00Z", after.PeriodStart)
	}
	if before.Responder.ID == after.Responder.ID {
		t.Errorf("handover across the DST boundary did not change responder (%s)", after.Responder.ID)
	}
	// Just before the summer handover the previous person still carries it.
	still, err := r.OnCall("weekly", at(t, "2026-03-30T07:59:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if still.Responder.ID != before.Responder.ID {
		t.Errorf("at 09:59 local on handover day on call = %s, want %s",
			still.Responder.ID, before.Responder.ID)
	}
}

func TestOverrideBeatsRotation(t *testing.T) {
	r := load(t)
	// 2026-01-06 is inside alice's first week.
	base, err := r.OnCall("covered", at(t, "2026-01-06T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if base.Responder.ID != "alice" || base.FromOverride {
		t.Fatalf("outside the override window got %s (override=%v)", base.Responder.ID, base.FromOverride)
	}
	in, err := r.OnCall("covered", at(t, "2026-01-06T22:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if in.Responder.ID != "carol" || !in.FromOverride {
		t.Fatalf("inside the override window got %s (override=%v)", in.Responder.ID, in.FromOverride)
	}
	if in.Reason != "manual override: alice at a wedding" {
		t.Errorf("reason = %q", in.Reason)
	}
	// The window is half-open: the instant it ends, the rotation resumes.
	out, err := r.OnCall("covered", at(t, "2026-01-07T08:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Responder.ID != "alice" || out.FromOverride {
		t.Fatalf("at the override end got %s (override=%v)", out.Responder.ID, out.FromOverride)
	}
}

func TestScheduleWalksForward(t *testing.T) {
	r := load(t)
	s, err := r.Schedule("weekly", at(t, "2026-01-05T09:00:00Z"), 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alice", "bob", "carol", "alice"}
	if len(s) != len(want) {
		t.Fatalf("got %d entries, want %d", len(s), len(want))
	}
	for i, w := range want {
		if s[i].Responder.ID != w {
			t.Errorf("entry %d = %s, want %s", i, s[i].Responder.ID, w)
		}
		if i > 0 && !s[i].PeriodStart.Equal(s[i-1].PeriodEnd) {
			t.Errorf("entry %d starts at %s but the previous ended at %s",
				i, s[i].PeriodStart, s[i-1].PeriodEnd)
		}
	}
}

func TestUnknownRota(t *testing.T) {
	if _, err := load(t).OnCall("nope", time.Now()); err == nil {
		t.Fatal("expected an error for an unknown rota")
	}
}
