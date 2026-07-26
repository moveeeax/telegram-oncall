// Package ingest normalises inbound alert payloads (Alertmanager webhook v4,
// Grafana unified alerting, and a generic JSON shape) into a single Alert type.
//
// Everything here is pure: no network, no Telegram, no database. The webhook
// transport that calls Parse lives outside this package.
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status is the lifecycle state reported by the alert source.
type Status string

const (
	StatusFiring   Status = "firing"
	StatusResolved Status = "resolved"
)

// Alert is the normalised representation every routing and escalation decision
// is made against.
type Alert struct {
	Source       string            `json:"source"`
	Status       Status            `json:"status"`
	Fingerprint  string            `json:"fingerprint"`
	Title        string            `json:"title"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"starts_at"`
	EndsAt       time.Time         `json:"ends_at,omitempty"`
	GeneratorURL string            `json:"generator_url,omitempty"`
}

// Severity returns the value of the `severity` label lowercased, or "unknown".
func (a Alert) Severity() string {
	for _, k := range []string{"severity", "Severity", "level"} {
		if v, ok := a.Labels[k]; ok && v != "" {
			return strings.ToLower(v)
		}
	}
	return "unknown"
}

// ComputeFingerprint derives a stable identity from the label set. It is used
// when the source does not supply one, and it is what dedup groups on.
func ComputeFingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\x00", k, labels[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Format identifies a supported inbound payload shape.
type Format string

const (
	FormatAlertmanager Format = "alertmanager"
	FormatGrafana      Format = "grafana"
	FormatGeneric      Format = "generic"
)

// Parse decodes body in the requested format. Passing an empty format runs
// Detect first.
func Parse(format Format, body []byte) ([]Alert, error) {
	if format == "" {
		var err error
		format, err = Detect(body)
		if err != nil {
			return nil, err
		}
	}
	switch format {
	case FormatAlertmanager:
		return parseAlertmanager(body)
	case FormatGrafana:
		return parseGrafana(body)
	case FormatGeneric:
		return parseGeneric(body)
	default:
		return nil, fmt.Errorf("ingest: unknown format %q", format)
	}
}

// Detect sniffs the payload shape.
//
// Alertmanager v4 and Grafana unified alerting are near-identical at the top
// level — both carry receiver, status, alerts, groupKey and truncatedAlerts.
// The reliable discriminators are Grafana's orgId and state fields, which
// Alertmanager never sends, and Alertmanager's version "4", which Grafana never
// sends (it reports version "1").
func Detect(body []byte) (Format, error) {
	var probe struct {
		Version  string            `json:"version"`
		Receiver string            `json:"receiver"`
		OrgID    *int64            `json:"orgId"`
		State    string            `json:"state"`
		Alerts   []json.RawMessage `json:"alerts"`
		Title    string            `json:"title"`
		Labels   map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", fmt.Errorf("ingest: payload is not JSON: %w", err)
	}
	switch {
	case probe.OrgID != nil || probe.State != "":
		return FormatGrafana, nil
	case probe.Version == "4" || (probe.Receiver != "" && len(probe.Alerts) > 0):
		return FormatAlertmanager, nil
	case probe.Labels != nil || probe.Title != "":
		return FormatGeneric, nil
	default:
		return "", fmt.Errorf("ingest: cannot detect payload format")
	}
}

func normStatus(s string) Status {
	if strings.EqualFold(s, "resolved") {
		return StatusResolved
	}
	return StatusFiring
}

// amAlert is the per-alert shape shared by Alertmanager v4 and Grafana.
type amAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

type amPayload struct {
	Version           string            `json:"version"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []amAlert         `json:"alerts"`
	Title             string            `json:"title"`
	Message           string            `json:"message"`
}

func (p amPayload) toAlerts(source string) ([]Alert, error) {
	if len(p.Alerts) == 0 {
		return nil, fmt.Errorf("ingest: %s payload contains no alerts", source)
	}
	out := make([]Alert, 0, len(p.Alerts))
	for i, a := range p.Alerts {
		labels := mergeMaps(p.CommonLabels, a.Labels)
		anns := mergeMaps(p.CommonAnnotations, a.Annotations)
		status := a.Status
		if status == "" {
			status = p.Status
		}
		fp := a.Fingerprint
		if fp == "" {
			fp = ComputeFingerprint(labels)
		}
		starts := a.StartsAt
		if starts.IsZero() {
			return nil, fmt.Errorf("ingest: %s alert %d has no startsAt", source, i)
		}
		out = append(out, Alert{
			Source:       source,
			Status:       normStatus(status),
			Fingerprint:  fp,
			Title:        alertTitle(labels, anns, p.Title),
			Labels:       labels,
			Annotations:  anns,
			StartsAt:     starts.UTC(),
			EndsAt:       zeroEnd(a.EndsAt),
			GeneratorURL: a.GeneratorURL,
		})
	}
	return out, nil
}

func parseAlertmanager(body []byte) ([]Alert, error) {
	var p amPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("ingest: invalid alertmanager payload: %w", err)
	}
	if p.Version != "" && p.Version != "4" {
		return nil, fmt.Errorf("ingest: unsupported alertmanager webhook version %q (want 4)", p.Version)
	}
	return p.toAlerts(string(FormatAlertmanager))
}

func parseGrafana(body []byte) ([]Alert, error) {
	var p amPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("ingest: invalid grafana payload: %w", err)
	}
	return p.toAlerts(string(FormatGrafana))
}

// genericPayload is the shape accepted on the generic JSON endpoint, for
// anything that is neither Prometheus nor Grafana (cron wrappers, custom
// exporters, a curl from a shell script).
type genericPayload struct {
	Status      string            `json:"status"`
	Title       string            `json:"title"`
	Severity    string            `json:"severity"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Fingerprint string            `json:"fingerprint"`
	StartsAt    *time.Time        `json:"starts_at"`
	URL         string            `json:"url"`
}

func parseGeneric(body []byte) ([]Alert, error) {
	var p genericPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("ingest: invalid generic payload: %w", err)
	}
	if p.Title == "" && len(p.Labels) == 0 {
		return nil, fmt.Errorf("ingest: generic payload needs at least a title or labels")
	}
	labels := mergeMaps(nil, p.Labels)
	if p.Severity != "" {
		labels["severity"] = strings.ToLower(p.Severity)
	}
	if p.Title != "" {
		if _, ok := labels["alertname"]; !ok {
			labels["alertname"] = p.Title
		}
	}
	fp := p.Fingerprint
	if fp == "" {
		fp = ComputeFingerprint(labels)
	}
	starts := time.Now().UTC()
	if p.StartsAt != nil {
		starts = p.StartsAt.UTC()
	}
	return []Alert{{
		Source:       string(FormatGeneric),
		Status:       normStatus(p.Status),
		Fingerprint:  fp,
		Title:        alertTitle(labels, p.Annotations, p.Title),
		Labels:       labels,
		Annotations:  mergeMaps(nil, p.Annotations),
		StartsAt:     starts,
		GeneratorURL: p.URL,
	}}, nil
}

func alertTitle(labels, anns map[string]string, fallback string) string {
	if v := anns["summary"]; v != "" {
		return v
	}
	if v := labels["alertname"]; v != "" {
		return v
	}
	if fallback != "" {
		return fallback
	}
	return "(untitled alert)"
}

func mergeMaps(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func zeroEnd(t time.Time) time.Time {
	// Alertmanager sends the zero-ish sentinel 0001-01-01 for unresolved alerts.
	if t.IsZero() || t.Year() <= 1 {
		return time.Time{}
	}
	return t.UTC()
}
