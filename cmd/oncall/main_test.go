package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ex resolves a path inside examples/, the same files the README tells people
// to run against.
func ex(name string) string { return filepath.Join("..", "..", "examples", name) }

// capture runs fn with os.Stdout redirected and returns what it printed.
func capture(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	errCh := make(chan error, 1)
	go func() { errCh <- fn() }()
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	runErr := <-errCh
	w.Close()
	os.Stdout = old
	out := <-done
	if runErr != nil {
		t.Fatalf("command failed: %v\noutput:\n%s", runErr, out)
	}
	return out
}

func TestValidateExampleConfig(t *testing.T) {
	out := capture(t, func() error { return cmdValidate([]string{"-c", ex("oncall.yaml")}) })
	if !strings.Contains(out, `ok: workspace "acme-sre"`) {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if strings.Contains(out, "no catch-all route") {
		t.Error("the example config does have a catch-all route")
	}
}

func TestWhoIsOnResolvesOverride(t *testing.T) {
	out := capture(t, func() error {
		return cmdWhoIsOn([]string{"-c", ex("oncall.yaml"), "-at", "2026-03-13T22:41:07Z", "-next", "2"})
	})
	if !strings.Contains(out, "manual override: alice at a wedding") {
		t.Fatalf("override not surfaced:\n%s", out)
	}
	if !strings.Contains(out, "secondary") {
		t.Fatalf("secondary rota missing:\n%s", out)
	}
}

func TestRouteExplainsTheLadder(t *testing.T) {
	out := capture(t, func() error {
		return cmdRoute([]string{"-c", ex("oncall.yaml"), "-f", ex("alertmanager-firing.json"),
			"-at", "2026-03-13T22:41:07Z"})
	})
	for _, want := range []string{
		"route=database", "target=db-topic(topic)", "escalation=db-fast",
		"breaks through quiet hours", "step 0: rota:primary",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// This is the README's headline example. If it stops producing a full timeline,
// the README is lying and this test fails.
func TestSimulateProducesAnAckedTimeline(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sim.db")
	out := capture(t, func() error {
		return cmdSimulate([]string{
			"-c", ex("oncall.yaml"), "-f", ex("alertmanager-firing.json"),
			"-ack-after", "3m", "-ack-by", "bob", "-until", "30m", "-db", db,
		})
	})
	for _, want := range []string{
		"T+00:00  ingest  opened",
		"T+00:00  [page] step=0 CRITICAL -> db-topic (@carol)",
		"buttons: ack:1 | resolve:1",
		"T+02:00  [escalate] step=1 CRITICAL -> db-topic (mention-all)",
		"T+03:00  [ack] incident 1 acknowledged by bob — ladder stopped",
		"status=acked",
		"acked_by=bob after 3m0s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// And the ledger it persisted must export cleanly.
	csvOut := capture(t, func() error { return cmdExport([]string{"-db", db, "-format", "csv"}) })
	lines := strings.Split(strings.TrimSpace(csvOut), "\n")
	if len(lines) != 5 {
		t.Fatalf("csv export has %d lines, want header + 4 rows:\n%s", len(lines), csvOut)
	}
	if lines[0] != "id,incident_id,at,actor,action,detail" {
		t.Errorf("csv header = %q", lines[0])
	}
	if !strings.Contains(lines[4], ",bob,ack,") {
		t.Errorf("last ledger row = %q, want bob's ack", lines[4])
	}

	jsonOut := capture(t, func() error { return cmdExport([]string{"-db", db, "-format", "json"}) })
	if !strings.Contains(jsonOut, `"Action": "ack"`) {
		t.Errorf("json export missing the ack entry:\n%s", jsonOut)
	}
}

func TestSimulateHeldByQuietHours(t *testing.T) {
	out := capture(t, func() error {
		return cmdSimulate([]string{"-c", ex("oncall.yaml"), "-f", ex("grafana-firing.json"), "-until", "30m"})
	})
	if !strings.Contains(out, "ingest  held") {
		t.Fatalf("a warning at 23:44 local should be held by quiet hours:\n%s", out)
	}
	if strings.Contains(out, "[page]") {
		t.Errorf("a held incident must not page:\n%s", out)
	}
}

func TestExportRequiresDB(t *testing.T) {
	if err := cmdExport([]string{"-format", "csv"}); err == nil {
		t.Fatal("expected an error when -db is missing")
	}
}
