// Command oncall is the offline control surface for a telegram-oncall
// workspace: validate the config, ask who is on call, dry-run routing for a
// real alert payload, replay a whole incident through the escalation state
// machine, export the acknowledgement ledger, and write the shift handoff.
//
// Nothing in this binary talks to Telegram, so none of it needs a bot token.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/clock"
	"github.com/moveeeax/telegram-oncall/internal/config"
	"github.com/moveeeax/telegram-oncall/internal/escalate"
	"github.com/moveeeax/telegram-oncall/internal/handoff"
	"github.com/moveeeax/telegram-oncall/internal/ingest"
	"github.com/moveeeax/telegram-oncall/internal/notify"
	"github.com/moveeeax/telegram-oncall/internal/rota"
	"github.com/moveeeax/telegram-oncall/internal/routing"
	"github.com/moveeeax/telegram-oncall/internal/store"
)

const usage = `oncall — offline control surface for a telegram-oncall workspace

Usage:
  oncall validate  -c CONFIG
  oncall whoison   -c CONFIG [-rota NAME] [-at RFC3339] [-next N]
  oncall route     -c CONFIG -f PAYLOAD [-source alertmanager|grafana|generic] [-at RFC3339]
  oncall simulate  -c CONFIG -f PAYLOAD [-ack-after 7m] [-ack-by alice] [-until 1h] [-db FILE]
  oncall export    -db FILE [-format csv|json]
  oncall handoff   -db FILE [-c CONFIG] [-rota NAME] [-since RFC3339] [-until RFC3339|now] [-format markdown|short]

Commands:
  validate  parse the YAML workspace and report every problem at once
  whoison   resolve the on-call responder for a rota, including overrides
  route     show which route, target and escalation an alert payload lands on
  simulate  replay an alert through the escalation state machine on a fake clock
  export    dump the acknowledgement ledger from a database written by simulate
  handoff   summarise a shift: what fired, what is still live, overrides ahead
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "whoison":
		err = cmdWhoIsOn(os.Args[2:])
	case "route":
		err = cmdRoute(os.Args[2:])
	case "simulate":
		err = cmdSimulate(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "handoff":
		err = cmdHandoff(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "oncall: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "oncall: %v\n", err)
		os.Exit(1)
	}
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	path := fs.String("c", "oncall.yaml", "path to the workspace YAML")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("ok: workspace %q\n", cfg.Workspace)
	fmt.Printf("  %d target(s), %d responder(s), %d rota(s), %d escalation(s), %d route(s), %d heartbeat(s)\n",
		len(cfg.Targets), len(cfg.Responders), len(cfg.Rotas), len(cfg.Escalations),
		len(cfg.Routes), len(cfg.Heartbeats))
	if cfg.QuietHours != nil {
		fmt.Printf("  quiet hours %s-%s %s, suppress %v, breakthrough %v\n",
			cfg.QuietHours.From, cfg.QuietHours.To, cfg.QuietHours.Timezone,
			cfg.QuietHours.Suppress, cfg.QuietHours.Breakthrough)
	}
	if !hasCatchAll(cfg) {
		fmt.Println("  warning: no catch-all route — alerts matching nothing are dropped")
	}
	return nil
}

func hasCatchAll(cfg *config.Config) bool {
	for _, r := range cfg.Routes {
		m := r.Match
		if len(m.Labels) == 0 && len(m.LabelsRe) == 0 && len(m.Severity) == 0 &&
			len(m.Absent) == 0 && len(m.SourceIn) == 0 {
			return true
		}
	}
	return false
}

func cmdWhoIsOn(args []string) error {
	fs := flag.NewFlagSet("whoison", flag.ExitOnError)
	path := fs.String("c", "oncall.yaml", "path to the workspace YAML")
	rotaName := fs.String("rota", "", "rota name (default: every rota)")
	atStr := fs.String("at", "", "RFC3339 instant to resolve at (default: now)")
	next := fs.Int("next", 0, "also print the next N handovers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	at, err := parseAt(*atStr)
	if err != nil {
		return err
	}
	res := rota.NewResolver(cfg)
	names := []string{*rotaName}
	if *rotaName == "" {
		names = names[:0]
		for _, r := range cfg.Rotas {
			names = append(names, r.Name)
		}
	}
	for _, n := range names {
		a, err := res.OnCall(n, at)
		if err != nil {
			return err
		}
		fmt.Printf("%-12s %s (%s)  until %s  [%s]\n", a.Rota, a.Responder.ID,
			a.Responder.Name, a.PeriodEnd.Format(time.RFC3339), a.Reason)
		if *next > 0 {
			sched, err := res.Schedule(n, a.PeriodEnd, *next)
			if err != nil {
				return err
			}
			for _, s := range sched {
				fmt.Printf("  %s -> %s  %s\n", s.PeriodStart.Format(time.RFC3339),
					s.PeriodEnd.Format(time.RFC3339), s.Responder.ID)
			}
		}
	}
	return nil
}

func cmdRoute(args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	path := fs.String("c", "oncall.yaml", "path to the workspace YAML")
	payload := fs.String("f", "", "alert payload JSON file ('-' for stdin)")
	source := fs.String("source", "", "payload format: alertmanager|grafana|generic (default: detect)")
	atStr := fs.String("at", "", "RFC3339 instant to evaluate quiet hours at (default: now)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, alerts, at, err := loadConfigAndAlerts(*path, *payload, *source, *atStr)
	if err != nil {
		return err
	}
	r := routing.New(cfg)
	res := rota.NewResolver(cfg)
	for _, a := range alerts {
		fmt.Printf("alert %s  fingerprint=%s severity=%s status=%s source=%s\n",
			a.Title, a.Fingerprint, a.Severity(), a.Status, a.Source)
		ds := r.Route(a, at)
		if len(ds) == 0 {
			fmt.Println("  UNROUTED — no route matched; this alert would be dropped")
			continue
		}
		for _, d := range ds {
			fmt.Printf("  route=%s target=%s(%s) escalation=%s\n",
				d.Route, d.Target.Name, d.Target.Type, d.Escalation)
			if d.Suppressed {
				fmt.Printf("    HELD: %s\n", d.Reason)
			} else if d.Reason != "" {
				fmt.Printf("    %s\n", d.Reason)
			}
			if esc, ok := cfg.Escalation(d.Escalation); ok {
				for i, s := range esc.Steps {
					who := describeStep(res, s, at)
					fmt.Printf("    step %d: %-22s %s\n", i, s.Notify, who)
				}
			}
		}
	}
	return nil
}

func describeStep(res *rota.Resolver, s config.Step, at time.Time) string {
	kind, arg, _ := strings.Cut(s.Notify, ":")
	suffix := ""
	if s.Wait > 0 {
		suffix = fmt.Sprintf(" then wait %s", s.Wait)
	}
	switch kind {
	case "rota":
		a, err := res.OnCall(arg, at)
		if err != nil {
			return "(" + err.Error() + ")"
		}
		return fmt.Sprintf("-> %s%s", a.Responder.ID, suffix)
	case "responder":
		return fmt.Sprintf("-> %s%s", arg, suffix)
	default:
		return "-> the group chat" + suffix
	}
}

func cmdSimulate(args []string) error {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	path := fs.String("c", "oncall.yaml", "path to the workspace YAML")
	payload := fs.String("f", "", "alert payload JSON file ('-' for stdin)")
	source := fs.String("source", "", "payload format: alertmanager|grafana|generic (default: detect)")
	atStr := fs.String("at", "", "RFC3339 start instant (default: the alert's startsAt)")
	ackAfter := fs.Duration("ack-after", 0, "acknowledge this long after the first page (0 = never)")
	ackBy := fs.String("ack-by", "", "responder id that presses Ack (default: whoever was paged)")
	until := fs.Duration("until", time.Hour, "how far to run the simulated clock")
	dbPath := fs.String("db", ":memory:", "SQLite file to persist state to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, alerts, at, err := loadConfigAndAlerts(*path, *payload, *source, *atStr)
	if err != nil {
		return err
	}
	if *atStr == "" && len(alerts) > 0 && !alerts[0].StartsAt.IsZero() {
		at = alerts[0].StartsAt
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	clk := clock.NewFake(at)
	rec := notify.NewRecorder()
	eng := escalate.New(cfg, st, rec, clk)
	ctx := context.Background()
	start := at

	// Timeline offsets read as MM:SS from the first alert, which is how people
	// describe an incident afterwards ("nobody acked for eleven minutes").
	rel := func(t time.Time) string {
		d := t.Sub(start).Round(time.Second)
		return fmt.Sprintf("T+%02d:%02d", int(d/time.Minute), int((d%time.Minute)/time.Second))
	}

	fmt.Printf("simulating from %s (workspace %q)\n\n", start.Format(time.RFC3339), cfg.Workspace)

	var incidentID int64
	for _, a := range alerts {
		outs, err := eng.Ingest(ctx, a)
		if err != nil {
			return err
		}
		for _, o := range outs {
			fmt.Printf("%s  ingest  %-9s route=%-12s %s\n", rel(clk.Now()), o.Action, o.Route, o.Reason)
			if o.IncidentID != 0 && incidentID == 0 {
				incidentID = o.IncidentID
			}
		}
	}
	if incidentID == 0 {
		fmt.Println("\nnothing was opened — nothing to escalate")
		return nil
	}

	deadline := start.Add(*until)
	ackAt := time.Time{}
	if *ackAfter > 0 {
		ackAt = start.Add(*ackAfter)
	}
	acked := false

	drain := func() error {
		pages, err := eng.Tick(ctx)
		if err != nil {
			return err
		}
		for _, p := range pages {
			fmt.Printf("%s  %s\n", rel(p.At), p.String())
			if len(p.Buttons()) > 0 {
				fmt.Printf("            buttons: %s\n", strings.Join(p.Buttons(), " | "))
			}
		}
		return nil
	}
	if err := drain(); err != nil {
		return err
	}

	for {
		if !acked && !ackAt.IsZero() && !ackAt.After(clk.Now()) {
			if err := doAck(ctx, eng, st, incidentID, *ackBy, rel, clk.Now()); err != nil {
				return err
			}
			acked = true
			continue
		}
		next, ok, err := eng.NextTimer(ctx)
		if err != nil {
			return err
		}
		jump := next
		if !ok {
			if acked || ackAt.IsZero() {
				break
			}
			jump = ackAt
		} else if !acked && !ackAt.IsZero() && ackAt.Before(next) {
			jump = ackAt
		}
		if jump.After(deadline) {
			break
		}
		if jump.After(clk.Now()) {
			clk.Set(jump)
		}
		if !acked && !ackAt.IsZero() && !ackAt.After(clk.Now()) {
			if err := doAck(ctx, eng, st, incidentID, *ackBy, rel, clk.Now()); err != nil {
				return err
			}
			acked = true
		}
		if err := drain(); err != nil {
			return err
		}
		if !ok && acked {
			break
		}
	}

	inc, err := st.Incident(ctx, incidentID)
	if err != nil {
		return err
	}
	fmt.Printf("\nincident %d: status=%s step=%d occurrences=%d", inc.ID, inc.Status, inc.Step, inc.Occurrences)
	if inc.AckedBy != "" {
		fmt.Printf(" acked_by=%s after %s", inc.AckedBy, inc.AckedAt.Sub(inc.CreatedAt).Round(time.Second))
	}
	fmt.Println()

	entries, err := st.Ledger(ctx, incidentID)
	if err != nil {
		return err
	}
	fmt.Println("\nack ledger:")
	for _, e := range entries {
		fmt.Printf("  %s  %-8s %-10s %s\n", e.At.Format(time.RFC3339), e.Actor, e.Action, e.Detail)
	}
	if *dbPath != ":memory:" {
		fmt.Printf("\nstate persisted to %s — try: oncall export -db %s -format csv\n", *dbPath, *dbPath)
	}
	return nil
}

func doAck(ctx context.Context, eng *escalate.Engine, st *store.Store, id int64, ackBy string,
	rel func(time.Time) string, now time.Time) error {
	actor := ackBy
	if actor == "" {
		actor = "on-call"
	}
	ok, err := eng.Ack(ctx, id, actor)
	if err != nil {
		return err
	}
	if ok {
		fmt.Printf("%s  [ack] incident %d acknowledged by %s — ladder stopped\n", rel(now), id, actor)
	}
	return nil
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dbPath := fs.String("db", "", "SQLite file to read")
	format := fs.String("format", "csv", "csv or json")
	incident := fs.Int64("incident", 0, "restrict to one incident id (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return fmt.Errorf("export: -db is required")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	entries, err := st.Ledger(context.Background(), *incident)
	if err != nil {
		return err
	}
	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	case "csv":
		w := csv.NewWriter(os.Stdout)
		defer w.Flush()
		if err := w.Write([]string{"id", "incident_id", "at", "actor", "action", "detail"}); err != nil {
			return err
		}
		for _, e := range entries {
			if err := w.Write([]string{
				fmt.Sprint(e.ID), fmt.Sprint(e.IncidentID), e.At.Format(time.RFC3339),
				e.Actor, e.Action, e.Detail,
			}); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("export: unknown format %q", *format)
	}
}

func cmdHandoff(args []string) error {
	fs := flag.NewFlagSet("handoff", flag.ExitOnError)
	dbPath := fs.String("db", "", "SQLite file to read")
	path := fs.String("c", "", "workspace YAML; adds the rota and override sections")
	rotaName := fs.String("rota", "", "rota whose shift ends at -until; sets the window when -since is empty")
	sinceStr := fs.String("since", "", "RFC3339 window start (default: start of the -rota shift)")
	untilStr := fs.String("until", "now", "RFC3339 window end, or now")
	format := fs.String("format", "markdown", "markdown or short")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return fmt.Errorf("handoff: -db is required")
	}
	if *format != "markdown" && *format != "short" {
		return fmt.Errorf("handoff: unknown format %q", *format)
	}
	if *rotaName != "" && *path == "" {
		return fmt.Errorf("handoff: -rota needs -c CONFIG")
	}
	var cfg *config.Config
	if *path != "" {
		var err error
		if cfg, err = config.Load(*path); err != nil {
			return err
		}
	}
	until := time.Now().UTC()
	if *untilStr != "now" {
		var err error
		if until, err = parseAt(*untilStr); err != nil {
			return err
		}
	}
	var win handoff.Window
	switch {
	case *sinceStr != "":
		since, err := parseAt(*sinceStr)
		if err != nil {
			return err
		}
		win = handoff.Window{Since: since, Until: until}
	case *rotaName != "":
		var err error
		if win, err = handoff.ShiftWindow(cfg, *rotaName, until); err != nil {
			return err
		}
	default:
		return fmt.Errorf("handoff: set -since, or -c and -rota to take the window from the rota")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := handoff.Build(context.Background(), st, handoff.Options{Config: cfg, Rota: *rotaName, Window: win})
	if err != nil {
		return err
	}
	if *format == "short" {
		return handoff.Short(os.Stdout, rep)
	}
	return handoff.Markdown(os.Stdout, rep)
}

func loadConfigAndAlerts(cfgPath, payload, source, atStr string) (*config.Config, []ingest.Alert, time.Time, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	if payload == "" {
		return nil, nil, time.Time{}, fmt.Errorf("-f PAYLOAD is required")
	}
	var body []byte
	if payload == "-" {
		body, err = io.ReadAll(os.Stdin)
	} else {
		body, err = os.ReadFile(payload)
	}
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	alerts, err := ingest.Parse(ingest.Format(source), body)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	at, err := parseAt(atStr)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return cfg, alerts, at, nil
}

func parseAt(s string) (time.Time, error) {
	if s == "" {
		return time.Now().UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid -at %q: want RFC3339 like 2026-03-13T22:41:00Z", s)
	}
	return t.UTC(), nil
}
