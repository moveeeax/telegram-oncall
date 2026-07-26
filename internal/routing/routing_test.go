package routing

import (
	"testing"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/config"
	"github.com/moveeeax/telegram-oncall/internal/ingest"
)

const routeYAML = `
workspace: test
timezone: Europe/Amsterdam
targets:
  - name: db-topic
    type: topic
    chat_id: "-100"
    thread_id: 7
  - name: sre-group
    type: group
    chat_id: "-100"
  - name: audit
    type: group
    chat_id: "-101"
responders:
  - id: alice
    telegram_user_id: 1
rotas:
  - name: primary
    start_date: "2026-01-05"
    members: [alice]
escalations:
  - name: e
    steps:
      - notify: target
routes:
  - name: audit-copy
    match:
      labels:
        team: db
    target: audit
    escalation: e
    continue: true
  - name: database
    match:
      labels:
        team: db
      labels_re:
        cluster: prod-.*
    target: db-topic
    escalation: e
  - name: paging
    match:
      severity: [critical, warning]
      absent: [silenced]
      source_in: [alertmanager]
    target: sre-group
    escalation: e
quiet_hours:
  from: "23:00"
  to: "08:00"
  suppress: [info, warning]
  breakthrough: [critical]
`

func router(t *testing.T) *Router {
	t.Helper()
	cfg, err := config.Parse([]byte(routeYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return New(cfg)
}

func alert(source string, labels map[string]string) ingest.Alert {
	return ingest.Alert{
		Source: source, Status: ingest.StatusFiring, Labels: labels,
		Fingerprint: ingest.ComputeFingerprint(labels), Title: "t",
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestContinueFallsThroughThenStops(t *testing.T) {
	r := router(t)
	// Daytime so quiet hours are irrelevant.
	noon := mustTime(t, "2026-03-13T12:00:00Z")
	ds := r.Route(alert("alertmanager", map[string]string{
		"team": "db", "cluster": "prod-eu-1", "severity": "critical",
	}), noon)
	if len(ds) != 2 {
		t.Fatalf("got %d decisions, want 2 (audit-copy then database)", len(ds))
	}
	if ds[0].Route != "audit-copy" || ds[1].Route != "database" {
		t.Fatalf("routes = %s, %s", ds[0].Route, ds[1].Route)
	}
	if ds[1].Target.ThreadID != 7 {
		t.Errorf("forum topic thread_id lost: %+v", ds[1].Target)
	}
}

func TestLabelRegexpIsAnchored(t *testing.T) {
	r := router(t)
	noon := mustTime(t, "2026-03-13T12:00:00Z")
	// "not-prod-eu-1" must not match `prod-.*`; without anchoring it would.
	ds := r.Route(alert("alertmanager", map[string]string{
		"team": "db", "cluster": "not-prod-eu-1", "severity": "critical",
	}), noon)
	for _, d := range ds {
		if d.Route == "database" {
			t.Fatal("unanchored regexp matched a substring")
		}
	}
}

func TestAbsentAndSourceMatchers(t *testing.T) {
	r := router(t)
	noon := mustTime(t, "2026-03-13T12:00:00Z")

	ds := r.Route(alert("alertmanager", map[string]string{"severity": "critical"}), noon)
	if len(ds) != 1 || ds[0].Route != "paging" {
		t.Fatalf("expected the paging route, got %+v", ds)
	}
	// A silenced label removes it from paging.
	ds = r.Route(alert("alertmanager", map[string]string{"severity": "critical", "silenced": "yes"}), noon)
	if len(ds) != 0 {
		t.Fatalf("absent matcher ignored: %+v", ds)
	}
	// So does the wrong source.
	ds = r.Route(alert("generic", map[string]string{"severity": "critical"}), noon)
	if len(ds) != 0 {
		t.Fatalf("source_in matcher ignored: %+v", ds)
	}
}

func TestUnroutedAlertReturnsNothing(t *testing.T) {
	r := router(t)
	ds := r.Route(alert("alertmanager", map[string]string{"team": "frontend", "severity": "info"}),
		mustTime(t, "2026-03-13T12:00:00Z"))
	if len(ds) != 0 {
		t.Fatalf("expected no route, got %+v", ds)
	}
}

func TestQuietHoursWrapMidnight(t *testing.T) {
	r := router(t)
	cases := []struct {
		utc      string // Europe/Amsterdam is UTC+1 in March before the 29th
		severity string
		want     bool
		note     string
	}{
		{"2026-03-13T22:30:00Z", "warning", true, "23:30 local, inside the window"},
		{"2026-03-13T22:30:00Z", "critical", false, "critical breaks through"},
		{"2026-03-14T02:00:00Z", "warning", true, "03:00 local, after midnight, still inside"},
		{"2026-03-14T07:30:00Z", "warning", false, "08:30 local, window has closed"},
		{"2026-03-13T21:59:00Z", "warning", false, "22:59 local, window has not opened"},
		{"2026-03-13T22:30:00Z", "unknown", false, "severity not listed as suppressed"},
	}
	for _, c := range cases {
		labels := map[string]string{"severity": c.severity}
		if c.severity == "unknown" {
			labels = map[string]string{}
		}
		labels["team"] = "db"
		labels["cluster"] = "prod-eu-1"
		ds := r.Route(alert("alertmanager", labels), mustTime(t, c.utc))
		if len(ds) == 0 {
			t.Fatalf("%s: alert did not route at all", c.note)
		}
		if ds[0].Suppressed != c.want {
			t.Errorf("%s: suppressed = %v, want %v (reason %q)",
				c.note, ds[0].Suppressed, c.want, ds[0].Reason)
		}
	}
}

func TestInQuietHoursIgnoresSeverity(t *testing.T) {
	r := router(t)
	if !r.InQuietHours(mustTime(t, "2026-03-14T02:00:00Z")) {
		t.Error("03:00 local should be inside quiet hours")
	}
	if r.InQuietHours(mustTime(t, "2026-03-13T12:00:00Z")) {
		t.Error("13:00 local should be outside quiet hours")
	}
}
