package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestDetectDistinguishesAlertmanagerFromGrafana(t *testing.T) {
	// Both fixtures carry receiver/status/alerts/truncatedAlerts, so this is the
	// case a naive sniffer gets wrong.
	cases := map[string]Format{
		"alertmanager-firing.json":   FormatAlertmanager,
		"alertmanager-resolved.json": FormatAlertmanager,
		"grafana-firing.json":        FormatGrafana,
		"generic.json":               FormatGeneric,
	}
	for name, want := range cases {
		got, err := Detect(fixture(t, name))
		if err != nil {
			t.Fatalf("%s: Detect: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: Detect = %q, want %q", name, got, want)
		}
	}
}

func TestParseAlertmanagerFiring(t *testing.T) {
	alerts, err := Parse("", fixture(t, "alertmanager-firing.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	a := alerts[0]
	if a.Status != StatusFiring {
		t.Errorf("status = %q, want firing", a.Status)
	}
	if a.Severity() != "critical" {
		t.Errorf("severity = %q, want critical", a.Severity())
	}
	if a.Fingerprint != "9f2a1c4b7d8e0a31" {
		t.Errorf("fingerprint = %q, want the source-supplied one", a.Fingerprint)
	}
	if a.Title != "Postgres replica is 412s behind primary" {
		t.Errorf("title = %q, want the summary annotation", a.Title)
	}
	// commonLabels must be merged under per-alert labels.
	if a.Labels["team"] != "db" {
		t.Errorf("labels[team] = %q, want db (from commonLabels)", a.Labels["team"])
	}
	if a.Labels["instance"] != "pg-replica-2:9187" {
		t.Errorf("labels[instance] = %q, want the per-alert value", a.Labels["instance"])
	}
	if !a.StartsAt.Equal(time.Date(2026, 3, 13, 22, 41, 7, 0, time.UTC)) {
		t.Errorf("startsAt = %s, want 2026-03-13T22:41:07Z", a.StartsAt)
	}
	if !a.EndsAt.IsZero() {
		t.Errorf("endsAt = %s, want zero for a firing alert (0001-01-01 sentinel)", a.EndsAt)
	}
	if a.Annotations["runbook_url"] == "" {
		t.Error("runbook_url annotation was dropped")
	}
}

func TestParseAlertmanagerResolved(t *testing.T) {
	alerts, err := Parse(FormatAlertmanager, fixture(t, "alertmanager-resolved.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := alerts[0]
	if a.Status != StatusResolved {
		t.Fatalf("status = %q, want resolved", a.Status)
	}
	if a.EndsAt.IsZero() {
		t.Error("endsAt must survive on a resolved alert")
	}
	// Firing and resolved for the same alert must share a fingerprint,
	// otherwise resolution never finds its incident.
	firing, err := Parse(FormatAlertmanager, fixture(t, "alertmanager-firing.json"))
	if err != nil {
		t.Fatalf("Parse firing: %v", err)
	}
	if a.Fingerprint != firing[0].Fingerprint {
		t.Errorf("resolved fingerprint %q != firing fingerprint %q", a.Fingerprint, firing[0].Fingerprint)
	}
}

func TestParseGrafana(t *testing.T) {
	alerts, err := Parse("", fixture(t, "grafana-firing.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := alerts[0]
	if a.Source != "grafana" {
		t.Errorf("source = %q, want grafana", a.Source)
	}
	if a.Severity() != "warning" {
		t.Errorf("severity = %q, want warning", a.Severity())
	}
	if a.Labels["service"] != "checkout" {
		t.Errorf("labels[service] = %q, want checkout", a.Labels["service"])
	}
}

func TestParseGeneric(t *testing.T) {
	alerts, err := Parse("", fixture(t, "generic.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := alerts[0]
	if a.Severity() != "critical" {
		t.Errorf("severity = %q, want critical (promoted from the top-level field)", a.Severity())
	}
	if a.Labels["alertname"] != "Backup job exited non-zero" {
		t.Errorf("alertname = %q, want the title promoted into labels", a.Labels["alertname"])
	}
	if a.Fingerprint == "" {
		t.Error("a generic payload without a fingerprint must get a computed one")
	}
}

func TestComputeFingerprintIsOrderIndependentAndDistinct(t *testing.T) {
	a := ComputeFingerprint(map[string]string{"alertname": "X", "cluster": "prod", "severity": "critical"})
	b := ComputeFingerprint(map[string]string{"severity": "critical", "cluster": "prod", "alertname": "X"})
	if a != b {
		t.Errorf("fingerprint depends on map order: %q != %q", a, b)
	}
	c := ComputeFingerprint(map[string]string{"alertname": "X", "cluster": "staging", "severity": "critical"})
	if a == c {
		t.Error("different label sets produced the same fingerprint")
	}
	// A value moved between keys must not collide.
	d := ComputeFingerprint(map[string]string{"a": "bc", "d": "e"})
	e := ComputeFingerprint(map[string]string{"a": "b", "cd": "e"})
	if d == e {
		t.Errorf("fingerprint is not delimiter-safe: %q collided", d)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := map[string][]byte{
		"not json":               []byte(`{`),
		"alertmanager v3":        []byte(`{"version":"3","receiver":"x","alerts":[{"status":"firing","labels":{}}]}`),
		"no alerts":              []byte(`{"version":"4","receiver":"x","alerts":[]}`),
		"alert without startsAt": []byte(`{"version":"4","receiver":"x","alerts":[{"status":"firing","labels":{"a":"b"}}]}`),
	}
	for name, body := range cases {
		if _, err := Parse(FormatAlertmanager, body); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
	if _, err := Parse(FormatGeneric, []byte(`{"status":"firing"}`)); err == nil {
		t.Error("generic payload with neither title nor labels should be rejected")
	}
	if _, err := Detect([]byte(`{"nothing":"useful"}`)); err == nil {
		t.Error("undetectable payload should be rejected")
	}
}
