// Package notify defines the outbound paging seam.
//
// The real transport is the Telegram Bot API, which needs a bot token and is
// therefore not implemented here. Everything upstream of this interface —
// ingest, routing, rotas, the escalation state machine — is exercised against
// the Recorder implementation, and the simulator prints its output.
package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/config"
)

// Kind classifies an outbound message.
type Kind string

const (
	KindPage     Kind = "page"     // first page for an incident
	KindEscalate Kind = "escalate" // a later rung of the ladder
	KindAck      Kind = "ack"      // acknowledgement confirmation
	KindResolve  Kind = "resolve"  // resolution notice
)

// Page is one outbound message with its inline Ack/Resolve buttons.
type Page struct {
	IncidentID  int64
	Kind        Kind
	Step        int
	Target      config.Target
	Responder   *config.Responder // set when the step pages a person
	Mention     bool              // @-ping everyone in the group
	Title       string
	Severity    string
	Fingerprint string
	At          time.Time
	Note        string
}

// Buttons returns the inline keyboard callback payloads the Telegram transport
// is expected to attach. Keeping them here means the state machine defines the
// contract and the transport only renders it.
func (p Page) Buttons() []string {
	if p.Kind == KindAck || p.Kind == KindResolve {
		return nil
	}
	return []string{
		fmt.Sprintf("ack:%d", p.IncidentID),
		fmt.Sprintf("resolve:%d", p.IncidentID),
	}
}

// String renders a one-line human summary, used by `oncall simulate`.
func (p Page) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] step=%d %s -> %s", p.Kind, p.Step, strings.ToUpper(p.Severity), p.Target.Name)
	if p.Responder != nil {
		fmt.Fprintf(&b, " (@%s)", p.Responder.ID)
	}
	if p.Mention {
		b.WriteString(" (mention-all)")
	}
	fmt.Fprintf(&b, ": %s", p.Title)
	if p.Note != "" {
		fmt.Fprintf(&b, " — %s", p.Note)
	}
	return b.String()
}

// Notifier delivers a Page. Implementations must be safe for concurrent use.
type Notifier interface {
	Send(ctx context.Context, p Page) error
}

// Recorder is an in-memory Notifier used by tests and by the simulator.
type Recorder struct {
	mu    sync.Mutex
	pages []Page
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Send records the page.
func (r *Recorder) Send(_ context.Context, p Page) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pages = append(r.pages, p)
	return nil
}

// Pages returns a copy of everything recorded so far.
func (r *Recorder) Pages() []Page {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Page, len(r.pages))
	copy(out, r.pages)
	return out
}

// Reset drops recorded pages.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pages = nil
}
