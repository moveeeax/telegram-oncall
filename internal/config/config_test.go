package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadExampleWorkspace(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "examples", "oncall.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Workspace != "acme-sre" {
		t.Errorf("workspace = %q", cfg.Workspace)
	}
	// Rotas inherit the workspace timezone and the default rotation.
	r, ok := cfg.Rota("primary")
	if !ok {
		t.Fatal("rota primary missing")
	}
	if r.Timezone != "Europe/Amsterdam" {
		t.Errorf("rota timezone = %q, want the inherited workspace timezone", r.Timezone)
	}
	if r.Rotation != RotationWeekly {
		t.Errorf("rotation = %q", r.Rotation)
	}
	if len(r.Overrides) != 1 || r.Overrides[0].Responder != "carol" {
		t.Fatalf("override not parsed: %+v", r.Overrides)
	}
	if !r.Overrides[0].From.Equal(time.Date(2026, 3, 13, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("override from = %s", r.Overrides[0].From)
	}
	esc, ok := cfg.Escalation("standard")
	if !ok {
		t.Fatal("escalation standard missing")
	}
	if len(esc.Steps) != 3 || esc.Steps[0].Wait.D() != 5*time.Minute {
		t.Fatalf("standard ladder not parsed: %+v", esc.Steps)
	}
	if !esc.Steps[2].Mention {
		t.Error("last step should @-ping the group")
	}
	if cfg.Dedup.FlapThreshold != 5 || cfg.Dedup.FlapWindow.D() != 30*time.Minute {
		t.Errorf("dedup = %+v", cfg.Dedup)
	}
	if cfg.QuietHours == nil || cfg.QuietHours.Timezone != "Europe/Amsterdam" {
		t.Errorf("quiet hours timezone not inherited: %+v", cfg.QuietHours)
	}
}

func TestDefaultsApplied(t *testing.T) {
	cfg, err := Parse([]byte(`
workspace: minimal
responders:
  - id: a
    telegram_user_id: 1
targets:
  - name: g
    type: group
    chat_id: "-100"
rotas:
  - name: r
    start_date: "2026-01-05"
    members: [a]
escalations:
  - name: e
    steps:
      - notify: target
routes:
  - name: all
    target: g
    escalation: e
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Timezone != "UTC" {
		t.Errorf("default timezone = %q", cfg.Timezone)
	}
	r, _ := cfg.Rota("r")
	if r.HandoverTime != "10:00" || r.Rotation != RotationWeekly {
		t.Errorf("rota defaults = %+v", r)
	}
	if cfg.Dedup.FlapThreshold != 5 {
		t.Errorf("default flap threshold = %d", cfg.Dedup.FlapThreshold)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Parse([]byte(`
workspace: ""
timezone: Mars/Olympus
targets:
  - name: t1
    type: topic
    chat_id: "-100"
  - name: t1
    type: teleport
    chat_id: "-100"
responders:
  - id: alice
    telegram_user_id: 0
rotas:
  - name: primary
    rotation: fortnightly
    handover_time: "25:00"
    start_date: "not-a-date"
    members: [alice, ghost]
    overrides:
      - responder: nobody
        from: 2026-03-14T00:00:00Z
        to: 2026-03-13T00:00:00Z
escalations:
  - name: std
    steps:
      - notify: rota:missing
        wait: 0s
      - notify: nonsense
routes:
  - name: r
    target: no-such-target
    escalation: no-such-escalation
    match:
      labels_re:
        team: "([unclosed"
      source_in: [carrier-pigeon]
quiet_hours:
  from: "9am"
  to: "08:00"
  suppress: [warning]
  breakthrough: [warning]
heartbeats:
  - name: hb
    interval: 0s
    target: nope
    escalation: nope
`))
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	all := strings.Join(ve.Problems, "\n")
	want := []string{
		"workspace: must be set",
		"unknown timezone \"Mars/Olympus\"",
		"targets[t1]: type topic requires thread_id",
		"duplicate target name",
		"type must be one of group|topic|dm",
		"responders[alice]: telegram_user_id must be set",
		"rotation must be daily or weekly",
		"handover_time must be HH:MM",
		"start_date must be YYYY-MM-DD",
		"member \"ghost\" is not a declared responder",
		"responder \"nobody\" is not declared",
		"`to` must be after `from`",
		"unknown rota \"missing\"",
		"notify must be target, rota:<name> or responder:<id>",
		"wait must be > 0 for every step but the last",
		"unknown target \"no-such-target\"",
		"unknown escalation \"no-such-escalation\"",
		"labels_re[team]",
		"unknown source \"carrier-pigeon\"",
		"quiet_hours.from: must be HH:MM",
		"both suppressed and breakthrough",
		"heartbeats[hb]: interval must be > 0",
	}
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("missing problem %q in:\n%s", w, all)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Parse([]byte("workspace: x\ntypo_field: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "typo_field") {
		t.Fatalf("want an error naming the unknown field, got %v", err)
	}
}

func TestBadDurationRejected(t *testing.T) {
	_, err := Parse([]byte(`
workspace: x
escalations:
  - name: e
    steps:
      - notify: target
        wait: "five minutes"
`))
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("want a duration error, got %v", err)
	}
}

func TestRoutesRequired(t *testing.T) {
	_, err := Parse([]byte("workspace: x\n"))
	if err == nil || !strings.Contains(err.Error(), "at least one route is required") {
		t.Fatalf("want a missing-routes error, got %v", err)
	}
}
