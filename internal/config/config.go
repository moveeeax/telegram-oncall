// Package config defines the declarative YAML document that describes a
// workspace: its Telegram targets, responders, rotas, routing rules, escalation
// policies, quiet hours and heartbeats.
//
// The document is the unit of API state — you PUT it, the server validates it,
// and every runtime decision is derived from it. Validation is deliberately
// strict and reports every problem at once, because the audience writes this
// file the way they write Terraform.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is a whole workspace declaration.
type Config struct {
	Workspace   string       `yaml:"workspace"`
	Timezone    string       `yaml:"timezone"`
	Targets     []Target     `yaml:"targets"`
	Responders  []Responder  `yaml:"responders"`
	Rotas       []Rota       `yaml:"rotas"`
	Escalations []Escalation `yaml:"escalations"`
	Routes      []Route      `yaml:"routes"`
	QuietHours  *QuietHours  `yaml:"quiet_hours"`
	Heartbeats  []Heartbeat  `yaml:"heartbeats"`
	Dedup       Dedup        `yaml:"dedup"`
}

// TargetType is where a page is delivered inside Telegram.
type TargetType string

const (
	TargetGroup TargetType = "group" // a normal group or supergroup
	TargetTopic TargetType = "topic" // a forum topic inside a supergroup
	TargetDM    TargetType = "dm"    // a direct message to a responder
)

// Target is a Telegram delivery destination.
type Target struct {
	Name     string     `yaml:"name"`
	Type     TargetType `yaml:"type"`
	ChatID   string     `yaml:"chat_id"`
	ThreadID int64      `yaml:"thread_id"`
}

// Responder is a human who can be paged.
type Responder struct {
	ID             string `yaml:"id"`
	Name           string `yaml:"name"`
	TelegramUserID int64  `yaml:"telegram_user_id"`
}

// Rotation is how a rota advances.
type Rotation string

const (
	RotationDaily  Rotation = "daily"
	RotationWeekly Rotation = "weekly"
)

// Override is a manual "I'm covering tonight" window that wins over the
// computed rotation.
type Override struct {
	Responder string    `yaml:"responder"`
	From      time.Time `yaml:"from"`
	To        time.Time `yaml:"to"`
	Reason    string    `yaml:"reason"`
}

// Rota is a rotation over an ordered list of responders.
type Rota struct {
	Name         string     `yaml:"name"`
	Timezone     string     `yaml:"timezone"`
	Rotation     Rotation   `yaml:"rotation"`
	HandoverTime string     `yaml:"handover_time"` // HH:MM in the rota timezone
	StartDate    string     `yaml:"start_date"`    // YYYY-MM-DD, first handover day
	Members      []string   `yaml:"members"`
	Overrides    []Override `yaml:"overrides"`
}

// Step is one rung of an escalation ladder. Notify is one of
// "rota:<name>", "responder:<id>", or "target" (page the route's target chat).
type Step struct {
	Notify  string   `yaml:"notify"`
	Wait    Duration `yaml:"wait"`
	Mention bool     `yaml:"mention"` // @-ping every group member on this step
}

// Escalation is a named ladder applied to an incident until it is acked.
type Escalation struct {
	Name  string `yaml:"name"`
	Steps []Step `yaml:"steps"`
}

// Match is the predicate a route applies to a normalised alert.
type Match struct {
	Labels   map[string]string `yaml:"labels"`    // exact label equality
	LabelsRe map[string]string `yaml:"labels_re"` // regexp label match
	Severity []string          `yaml:"severity"`  // any-of on the severity label
	Absent   []string          `yaml:"absent"`    // labels that must not be present
	SourceIn []string          `yaml:"source_in"` // alertmanager|grafana|generic
}

// Route binds matching alerts to a target and an escalation policy.
type Route struct {
	Name       string `yaml:"name"`
	Match      Match  `yaml:"match"`
	Target     string `yaml:"target"`
	Escalation string `yaml:"escalation"`
	Continue   bool   `yaml:"continue"` // keep evaluating later routes too
}

// QuietHours suppresses low-severity pages overnight. Severities listed in
// Breakthrough always page.
type QuietHours struct {
	Timezone     string   `yaml:"timezone"`
	From         string   `yaml:"from"` // HH:MM
	To           string   `yaml:"to"`   // HH:MM, may wrap past midnight
	Suppress     []string `yaml:"suppress"`
	Breakthrough []string `yaml:"breakthrough"`
}

// Heartbeat is a dead-man's switch: if the named job stops checking in within
// Interval+Grace, the rota is paged.
type Heartbeat struct {
	Name       string   `yaml:"name"`
	Interval   Duration `yaml:"interval"`
	Grace      Duration `yaml:"grace"`
	Target     string   `yaml:"target"`
	Escalation string   `yaml:"escalation"`
	Severity   string   `yaml:"severity"`
}

// Dedup tunes fingerprint grouping and flap suppression.
type Dedup struct {
	// FlapWindow is the period over which repeated firing/resolved cycles for
	// one fingerprint are counted.
	FlapWindow Duration `yaml:"flap_window"`
	// FlapThreshold is how many firings inside FlapWindow mark a fingerprint as
	// flapping; flapping fingerprints are recorded but not paged.
	FlapThreshold int `yaml:"flap_threshold"`
}

// Duration is a YAML-friendly time.Duration ("5m", "24h").
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders the duration.
func (d Duration) String() string { return time.Duration(d).String() }

// Load reads and validates a YAML config from disk.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes and validates a YAML config.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if c.Dedup.FlapWindow == 0 {
		c.Dedup.FlapWindow = Duration(30 * time.Minute)
	}
	if c.Dedup.FlapThreshold == 0 {
		c.Dedup.FlapThreshold = 5
	}
	for i := range c.Rotas {
		if c.Rotas[i].Timezone == "" {
			c.Rotas[i].Timezone = c.Timezone
		}
		if c.Rotas[i].Rotation == "" {
			c.Rotas[i].Rotation = RotationWeekly
		}
		if c.Rotas[i].HandoverTime == "" {
			c.Rotas[i].HandoverTime = "10:00"
		}
	}
	if c.QuietHours != nil && c.QuietHours.Timezone == "" {
		c.QuietHours.Timezone = c.Timezone
	}
	for i := range c.Heartbeats {
		if c.Heartbeats[i].Severity == "" {
			c.Heartbeats[i].Severity = "critical"
		}
		if c.Heartbeats[i].Grace == 0 {
			c.Heartbeats[i].Grace = Duration(5 * time.Minute)
		}
	}
}

// Target looks up a target by name.
func (c *Config) Target(name string) (Target, bool) {
	for _, t := range c.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// Rota looks up a rota by name.
func (c *Config) Rota(name string) (Rota, bool) {
	for _, r := range c.Rotas {
		if r.Name == name {
			return r, true
		}
	}
	return Rota{}, false
}

// Responder looks up a responder by id.
func (c *Config) Responder(id string) (Responder, bool) {
	for _, r := range c.Responders {
		if r.ID == id {
			return r, true
		}
	}
	return Responder{}, false
}

// Escalation looks up an escalation policy by name.
func (c *Config) Escalation(name string) (Escalation, bool) {
	for _, e := range c.Escalations {
		if e.Name == name {
			return e, true
		}
	}
	return Escalation{}, false
}

// ValidationError collects every problem found in one document.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return fmt.Sprintf("config invalid (%d problem(s)):\n  - %s",
		len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// Validate checks structural integrity and every cross reference.
func (c *Config) Validate() error {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }

	if c.Workspace == "" {
		add("workspace: must be set")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		add("timezone: unknown timezone %q", c.Timezone)
	}

	targets := map[string]bool{}
	for i, t := range c.Targets {
		if t.Name == "" {
			add("targets[%d]: name must be set", i)
		} else if targets[t.Name] {
			add("targets[%d]: duplicate target name %q", i, t.Name)
		}
		targets[t.Name] = true
		switch t.Type {
		case TargetGroup, TargetDM:
		case TargetTopic:
			if t.ThreadID == 0 {
				add("targets[%s]: type topic requires thread_id", t.Name)
			}
		default:
			add("targets[%s]: type must be one of group|topic|dm, got %q", t.Name, t.Type)
		}
		if t.ChatID == "" && t.Type != TargetDM {
			add("targets[%s]: chat_id must be set", t.Name)
		}
	}

	responders := map[string]bool{}
	for i, r := range c.Responders {
		if r.ID == "" {
			add("responders[%d]: id must be set", i)
			continue
		}
		if responders[r.ID] {
			add("responders[%d]: duplicate responder id %q", i, r.ID)
		}
		responders[r.ID] = true
		if r.TelegramUserID == 0 {
			add("responders[%s]: telegram_user_id must be set", r.ID)
		}
	}

	rotas := map[string]bool{}
	for i, r := range c.Rotas {
		if r.Name == "" {
			add("rotas[%d]: name must be set", i)
			continue
		}
		if rotas[r.Name] {
			add("rotas[%d]: duplicate rota name %q", i, r.Name)
		}
		rotas[r.Name] = true
		if r.Rotation != RotationDaily && r.Rotation != RotationWeekly {
			add("rotas[%s]: rotation must be daily or weekly, got %q", r.Name, r.Rotation)
		}
		if _, err := time.LoadLocation(r.Timezone); err != nil {
			add("rotas[%s]: unknown timezone %q", r.Name, r.Timezone)
		}
		if _, err := time.Parse("15:04", r.HandoverTime); err != nil {
			add("rotas[%s]: handover_time must be HH:MM, got %q", r.Name, r.HandoverTime)
		}
		if r.StartDate == "" {
			add("rotas[%s]: start_date must be set (YYYY-MM-DD)", r.Name)
		} else if _, err := time.Parse("2006-01-02", r.StartDate); err != nil {
			add("rotas[%s]: start_date must be YYYY-MM-DD, got %q", r.Name, r.StartDate)
		}
		if len(r.Members) == 0 {
			add("rotas[%s]: members must not be empty", r.Name)
		}
		for _, m := range r.Members {
			if !responders[m] {
				add("rotas[%s]: member %q is not a declared responder", r.Name, m)
			}
		}
		for j, o := range r.Overrides {
			if !responders[o.Responder] {
				add("rotas[%s].overrides[%d]: responder %q is not declared", r.Name, j, o.Responder)
			}
			if !o.To.After(o.From) {
				add("rotas[%s].overrides[%d]: `to` must be after `from`", r.Name, j)
			}
		}
	}

	escalations := map[string]bool{}
	for i, e := range c.Escalations {
		if e.Name == "" {
			add("escalations[%d]: name must be set", i)
			continue
		}
		if escalations[e.Name] {
			add("escalations[%d]: duplicate escalation name %q", i, e.Name)
		}
		escalations[e.Name] = true
		if len(e.Steps) == 0 {
			add("escalations[%s]: needs at least one step", e.Name)
		}
		for j, s := range e.Steps {
			kind, arg, _ := strings.Cut(s.Notify, ":")
			switch kind {
			case "target":
			case "rota":
				if !rotas[arg] {
					add("escalations[%s].steps[%d]: unknown rota %q", e.Name, j, arg)
				}
			case "responder":
				if !responders[arg] {
					add("escalations[%s].steps[%d]: unknown responder %q", e.Name, j, arg)
				}
			default:
				add("escalations[%s].steps[%d]: notify must be target, rota:<name> or responder:<id>, got %q",
					e.Name, j, s.Notify)
			}
			if j < len(e.Steps)-1 && s.Wait <= 0 {
				add("escalations[%s].steps[%d]: wait must be > 0 for every step but the last", e.Name, j)
			}
		}
	}

	if len(c.Routes) == 0 {
		add("routes: at least one route is required, otherwise nothing is deliverable")
	}
	routeNames := map[string]bool{}
	for i, r := range c.Routes {
		name := r.Name
		if name == "" {
			add("routes[%d]: name must be set", i)
			name = fmt.Sprintf("#%d", i)
		} else if routeNames[r.Name] {
			add("routes[%d]: duplicate route name %q", i, r.Name)
		}
		routeNames[name] = true
		if !targets[r.Target] {
			add("routes[%s]: unknown target %q", name, r.Target)
		}
		if !escalations[r.Escalation] {
			add("routes[%s]: unknown escalation %q", name, r.Escalation)
		}
		for k, expr := range r.Match.LabelsRe {
			if _, err := compileRe(expr); err != nil {
				add("routes[%s].match.labels_re[%s]: %v", name, k, err)
			}
		}
		for _, s := range r.Match.SourceIn {
			switch s {
			case "alertmanager", "grafana", "generic":
			default:
				add("routes[%s].match.source_in: unknown source %q", name, s)
			}
		}
	}

	if q := c.QuietHours; q != nil {
		if _, err := time.LoadLocation(q.Timezone); err != nil {
			add("quiet_hours: unknown timezone %q", q.Timezone)
		}
		if _, err := time.Parse("15:04", q.From); err != nil {
			add("quiet_hours.from: must be HH:MM, got %q", q.From)
		}
		if _, err := time.Parse("15:04", q.To); err != nil {
			add("quiet_hours.to: must be HH:MM, got %q", q.To)
		}
		if len(q.Suppress) == 0 {
			add("quiet_hours.suppress: list at least one severity, or drop the block")
		}
		for _, s := range q.Breakthrough {
			for _, sup := range q.Suppress {
				if s == sup {
					add("quiet_hours: severity %q is both suppressed and breakthrough", s)
				}
			}
		}
	}

	hb := map[string]bool{}
	for i, h := range c.Heartbeats {
		if h.Name == "" {
			add("heartbeats[%d]: name must be set", i)
			continue
		}
		if hb[h.Name] {
			add("heartbeats[%d]: duplicate heartbeat name %q", i, h.Name)
		}
		hb[h.Name] = true
		if h.Interval <= 0 {
			add("heartbeats[%s]: interval must be > 0", h.Name)
		}
		if !targets[h.Target] {
			add("heartbeats[%s]: unknown target %q", h.Name, h.Target)
		}
		if !escalations[h.Escalation] {
			add("heartbeats[%s]: unknown escalation %q", h.Name, h.Escalation)
		}
	}

	if len(p) > 0 {
		sort.Strings(p)
		return &ValidationError{Problems: p}
	}
	return nil
}
