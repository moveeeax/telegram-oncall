package handoff

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/store"
)

// Short-form limits. The short form is meant to fit in one chat message, so it
// lists the most important items and counts the rest.
const (
	shortMaxLive  = 10
	shortMaxFired = 5
	shortMaxTitle = 120
)

// Markdown renders the full report for a wiki page or a PR-style review.
func Markdown(w io.Writer, r Report) error {
	var b strings.Builder
	if r.Workspace != "" {
		fmt.Fprintf(&b, "# On-call handoff: %s\n\n", r.Workspace)
	} else {
		b.WriteString("# On-call handoff\n\n")
	}
	fmt.Fprintf(&b, "Window: %s to %s (%s)\n", ts(r.Window.Since), ts(r.Window.Until),
		r.Window.Until.Sub(r.Window.Since))

	if len(r.Shifts) > 0 {
		b.WriteString("\n## Rota\n\n")
		for _, s := range r.Shifts {
			fmt.Fprintf(&b, "- %s: %s hands over to %s, on call until %s [%s]\n",
				s.Rota, who(s.Outgoing.ID, s.Outgoing.Name), who(s.Incoming.ID, s.Incoming.Name),
				ts(s.IncomingUntil), s.Reason)
		}
	}

	b.WriteString("\n## Summary\n\n")
	b.WriteString(countsLine(r.Counts) + "\n")

	b.WriteString("\n## Fired in window\n\n")
	if len(r.Fired) == 0 {
		b.WriteString("Nothing fired.\n")
	} else {
		b.WriteString("| Severity | Alert | Route | Incidents | Firings | Status at handoff |\n")
		b.WriteString("| --- | --- | --- | ---: | ---: | --- |\n")
		for _, g := range r.Fired {
			status := string(g.LastStatus)
			if g.Flapping {
				status += ", flapping"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %s |\n", strings.ToUpper(g.Severity),
				cell(g.Title), cell(g.Route), g.Incidents, g.Firings, status)
		}
	}

	b.WriteString("\n## Still live at handoff\n\n")
	if len(r.Live) == 0 {
		b.WriteString("Nothing is open.\n")
	}
	for _, l := range r.Live {
		fmt.Fprintf(&b, "- #%d %s %s (route %s): %s\n", l.ID, strings.ToUpper(l.Severity),
			l.Title, l.Route, liveState(l))
	}

	if len(r.Shifts) > 0 {
		b.WriteString("\n## Overrides for the incoming shift\n\n")
		if len(r.Overrides) == 0 {
			b.WriteString("None.\n")
		}
		for _, o := range r.Overrides {
			fmt.Fprintf(&b, "- %s: %s covers %s to %s", o.Rota, o.Responder, ts(o.From), ts(o.To))
			if o.Reason != "" {
				fmt.Fprintf(&b, " (%s)", o.Reason)
			}
			b.WriteString("\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Short renders a plain-text summary sized for one Telegram message. It uses
// no markup, so it can be sent without a parse mode.
func Short(w io.Writer, r Report) error {
	var b strings.Builder
	b.WriteString("Handoff")
	if r.Workspace != "" {
		b.WriteString(" " + r.Workspace)
	}
	fmt.Fprintf(&b, " %s to %s\n", ts(r.Window.Since), ts(r.Window.Until))
	for _, s := range r.Shifts {
		fmt.Fprintf(&b, "%s: %s -> %s until %s\n", s.Rota, s.Outgoing.ID, s.Incoming.ID, ts(s.IncomingUntil))
	}
	firings := 0
	for _, g := range r.Fired {
		firings += g.Firings
	}
	fmt.Fprintf(&b, "Fired: %d alert(s), %d firing(s). Still live: %d. Acks: %d.\n",
		len(r.Fired), firings, len(r.Live), r.Counts.Acks)

	if len(r.Live) > 0 {
		b.WriteString("Live:\n")
		for i, l := range r.Live {
			if i == shortMaxLive {
				fmt.Fprintf(&b, "  +%d more\n", len(r.Live)-shortMaxLive)
				break
			}
			fmt.Fprintf(&b, "  #%d %s %s (%s)\n", l.ID, strings.ToUpper(l.Severity),
				clip(l.Title), liveState(l))
		}
	}

	if len(r.Fired) > 0 {
		// Noisiest first; ties keep the report's severity/route/fingerprint
		// order so the output stays stable.
		top := append([]Group(nil), r.Fired...)
		sort.SliceStable(top, func(i, j int) bool { return top[i].Firings > top[j].Firings })
		b.WriteString("Fired:\n")
		for i, g := range top {
			if i == shortMaxFired {
				fmt.Fprintf(&b, "  +%d more\n", len(top)-shortMaxFired)
				break
			}
			fmt.Fprintf(&b, "  %dx %s %s (%s)\n", g.Firings, strings.ToUpper(g.Severity),
				clip(g.Title), g.LastStatus)
		}
	}
	for _, o := range r.Overrides {
		fmt.Fprintf(&b, "Override %s: %s %s to %s\n", o.Rota, o.Responder, ts(o.From), ts(o.To))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func countsLine(c Counts) string {
	return fmt.Sprintf("%d opened, %d held, %d deduped, %d page(s), %d escalation(s), %d ack(s), %d resolve(s)",
		c.Opened, c.Held, c.Deduped, c.Pages, c.Escalations, c.Acks, c.Resolves)
}

func liveState(l Live) string {
	switch l.Status {
	case store.StatusAcked:
		return fmt.Sprintf("acked by %s, open since %s", l.AckedBy, ts(l.Since))
	case store.StatusHeld:
		return fmt.Sprintf("held since %s: %s", ts(l.Since), l.HeldReason)
	default:
		return fmt.Sprintf("NOT ACKED, open since %s", ts(l.Since))
	}
}

func who(id, name string) string {
	if name == "" || name == id {
		return id
	}
	return fmt.Sprintf("%s (%s)", id, name)
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// cell escapes a value for a Markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}

func clip(s string) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= shortMaxTitle {
		return string(r)
	}
	return string(r[:shortMaxTitle-3]) + "..."
}
