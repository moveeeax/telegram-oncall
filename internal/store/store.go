// Package store is the durable state behind the escalation state machine.
//
// Every timer lives in the database, not in a goroutine. A process restart
// loses nothing: the next Tick picks up whatever was due while the process was
// down, and the unique constraints below make each escalation step fire exactly
// once even if two workers tick concurrently.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so CI stays boring
)

// Status is an incident lifecycle state.
type Status string

const (
	StatusOpen     Status = "open"
	StatusAcked    Status = "acked"
	StatusResolved Status = "resolved"
	// StatusHeld means the incident was recorded but deliberately not paged
	// (quiet hours or flap suppression).
	StatusHeld Status = "held"
)

// TimerState is the lifecycle of a durable escalation timer.
type TimerState string

const (
	TimerPending   TimerState = "pending"
	TimerFired     TimerState = "fired"
	TimerCancelled TimerState = "cancelled"
)

// Incident is one paged (or held) alert group.
type Incident struct {
	ID          int64
	Fingerprint string
	Route       string
	Escalation  string
	Target      string
	Title       string
	Severity    string
	Status      Status
	Step        int
	Occurrences int
	Flapping    bool
	Labels      map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	AckedAt     *time.Time
	AckedBy     string
	ResolvedAt  *time.Time
	ResolvedBy  string
	HeldReason  string
}

// Timer is a durable escalation timer.
type Timer struct {
	ID         int64
	IncidentID int64
	Step       int
	FireAt     time.Time
	State      TimerState
}

// LedgerEntry is one immutable audit row: who did what, when.
type LedgerEntry struct {
	ID         int64
	IncidentID int64
	At         time.Time
	Actor      string
	Action     string
	Detail     string
}

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// ErrNotFound is returned when an incident id does not exist.
var ErrNotFound = errors.New("store: not found")

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS incidents (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	fingerprint  TEXT NOT NULL,
	route        TEXT NOT NULL,
	escalation   TEXT NOT NULL,
	target       TEXT NOT NULL,
	title        TEXT NOT NULL,
	severity     TEXT NOT NULL,
	status       TEXT NOT NULL,
	step         INTEGER NOT NULL DEFAULT 0,
	occurrences  INTEGER NOT NULL DEFAULT 1,
	flapping     INTEGER NOT NULL DEFAULT 0,
	labels       TEXT NOT NULL DEFAULT '{}',
	held_reason  TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	acked_at     INTEGER,
	acked_by     TEXT NOT NULL DEFAULT '',
	resolved_at  INTEGER,
	resolved_by  TEXT NOT NULL DEFAULT ''
);

-- At most one live incident per (fingerprint, route). This is what makes
-- dedup a database invariant rather than an application convention.
CREATE UNIQUE INDEX IF NOT EXISTS incidents_live
	ON incidents(fingerprint, route)
	WHERE status IN ('open', 'acked', 'held');

CREATE TABLE IF NOT EXISTS timers (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
	step        INTEGER NOT NULL,
	fire_at     INTEGER NOT NULL,
	state       TEXT NOT NULL,
	UNIQUE(incident_id, step)
);
CREATE INDEX IF NOT EXISTS timers_due ON timers(state, fire_at);

-- One delivery row per (incident, step): the exactly-once guard.
CREATE TABLE IF NOT EXISTS deliveries (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
	step        INTEGER NOT NULL,
	target      TEXT NOT NULL,
	responder   TEXT NOT NULL DEFAULT '',
	at          INTEGER NOT NULL,
	UNIQUE(incident_id, step)
);

CREATE TABLE IF NOT EXISTS ledger (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
	at          INTEGER NOT NULL,
	actor       TEXT NOT NULL,
	action      TEXT NOT NULL,
	detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ledger_incident ON ledger(incident_id, at);

-- Firing events, kept to compute flap rate over a window.
CREATE TABLE IF NOT EXISTS firings (
	fingerprint TEXT NOT NULL,
	at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS firings_fp ON firings(fingerprint, at);

CREATE TABLE IF NOT EXISTS heartbeats (
	name       TEXT PRIMARY KEY,
	last_seen  INTEGER NOT NULL,
	missed     INTEGER NOT NULL DEFAULT 0
);
`

// memSeq names successive in-memory databases so two Open(":memory:") calls in
// one process get independent state instead of silently sharing a cache.
var memSeq atomic.Int64

// Open opens (and migrates) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		// A shared cache keeps the in-memory database alive across the pool's
		// connections; without it each connection sees an empty database.
		dsn = fmt.Sprintf("file:oncall-mem-%d?mode=memory&cache=shared", memSeq.Add(1))
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite writer serialisation; keeps Tick simple
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for transactions owned by callers.
func (s *Store) DB() *sql.DB { return s.db }

func unix(t time.Time) int64 { return t.UTC().Unix() }

func fromUnix(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

// CreateIncident inserts a new incident. If a live incident already exists for
// the same (fingerprint, route) it returns that one with dedup=true and bumps
// its occurrence counter instead of creating a duplicate.
func (s *Store) CreateIncident(ctx context.Context, in Incident) (inc Incident, dedup bool, err error) {
	labels, _ := json.Marshal(in.Labels)
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO incidents
			(fingerprint, route, escalation, target, title, severity, status, step,
			 occurrences, flapping, labels, held_reason, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,0,1,?,?,?,?,?)`,
		in.Fingerprint, in.Route, in.Escalation, in.Target, in.Title, in.Severity,
		string(in.Status), boolInt(in.Flapping), string(labels), in.HeldReason,
		unix(in.CreatedAt), unix(in.CreatedAt))
	if err != nil {
		return Incident{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		existing, err := s.LiveIncident(ctx, in.Fingerprint, in.Route)
		if err != nil {
			return Incident{}, false, err
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE incidents SET occurrences = occurrences + 1, updated_at = ? WHERE id = ?`,
			unix(in.CreatedAt), existing.ID); err != nil {
			return Incident{}, false, err
		}
		existing.Occurrences++
		return existing, true, nil
	}
	id, _ := res.LastInsertId()
	created, err := s.Incident(ctx, id)
	return created, false, err
}

// LiveIncident returns the open/acked/held incident for a fingerprint+route.
func (s *Store) LiveIncident(ctx context.Context, fingerprint, route string) (Incident, error) {
	row := s.db.QueryRowContext(ctx, incidentCols+`
		WHERE fingerprint = ? AND route = ? AND status IN ('open','acked','held')`,
		fingerprint, route)
	return scanIncident(row)
}

// Incident loads one incident by id.
func (s *Store) Incident(ctx context.Context, id int64) (Incident, error) {
	inc, err := scanIncident(s.db.QueryRowContext(ctx, incidentCols+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, ErrNotFound
	}
	if err != nil {
		return Incident{}, err
	}
	return inc, nil
}

const incidentCols = `
	SELECT id, fingerprint, route, escalation, target, title, severity, status, step,
	       occurrences, flapping, labels, held_reason, created_at, updated_at,
	       acked_at, acked_by, resolved_at, resolved_by
	FROM incidents`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanIncident(r rowScanner) (Incident, error) {
	var (
		inc                 Incident
		status, labels      string
		created, updated    int64
		ackedAt, resolvedAt sql.NullInt64
		flapping            int
	)
	err := r.Scan(&inc.ID, &inc.Fingerprint, &inc.Route, &inc.Escalation, &inc.Target,
		&inc.Title, &inc.Severity, &status, &inc.Step, &inc.Occurrences, &flapping,
		&labels, &inc.HeldReason, &created, &updated, &ackedAt, &inc.AckedBy,
		&resolvedAt, &inc.ResolvedBy)
	if err != nil {
		return Incident{}, err
	}
	inc.Status = Status(status)
	inc.Flapping = flapping != 0
	inc.CreatedAt = time.Unix(created, 0).UTC()
	inc.UpdatedAt = time.Unix(updated, 0).UTC()
	inc.AckedAt = fromUnix(ackedAt)
	inc.ResolvedAt = fromUnix(resolvedAt)
	_ = json.Unmarshal([]byte(labels), &inc.Labels)
	return inc, nil
}

// ListIncidents returns incidents newest first, optionally filtered by status.
func (s *Store) ListIncidents(ctx context.Context, status Status) ([]Incident, error) {
	q := incidentCols
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, string(status))
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// IncidentsLiveAt returns every incident that existed at t and was not yet
// resolved by then, oldest first. "At t" is half-open: an incident created at
// exactly t, or resolved at exactly t, belongs to whoever is on call from t.
func (s *Store) IncidentsLiveAt(ctx context.Context, t time.Time) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx, incidentCols+`
		WHERE created_at < ? AND (resolved_at IS NULL OR resolved_at >= ?)
		ORDER BY created_at, id`, unix(t), unix(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// ScheduleTimer records a durable escalation timer. It is idempotent per
// (incident, step), so a retried Tick cannot double-schedule.
func (s *Store) ScheduleTimer(ctx context.Context, incidentID int64, step int, fireAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO timers (incident_id, step, fire_at, state) VALUES (?,?,?,?)`,
		incidentID, step, unix(fireAt), string(TimerPending))
	return err
}

// DueTimers returns pending timers whose fire_at has passed.
func (s *Store) DueTimers(ctx context.Context, now time.Time) ([]Timer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, incident_id, step, fire_at, state FROM timers
		 WHERE state = 'pending' AND fire_at <= ? ORDER BY fire_at, id`, unix(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Timer
	for rows.Next() {
		var t Timer
		var fa int64
		var st string
		if err := rows.Scan(&t.ID, &t.IncidentID, &t.Step, &fa, &st); err != nil {
			return nil, err
		}
		t.FireAt = time.Unix(fa, 0).UTC()
		t.State = TimerState(st)
		out = append(out, t)
	}
	return out, rows.Err()
}

// NextTimer returns the earliest pending timer, used by the simulator to jump
// the fake clock forward instead of polling.
func (s *Store) NextTimer(ctx context.Context) (Timer, bool, error) {
	var t Timer
	var fa int64
	var st string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, incident_id, step, fire_at, state FROM timers
		 WHERE state = 'pending' ORDER BY fire_at, id LIMIT 1`).
		Scan(&t.ID, &t.IncidentID, &t.Step, &fa, &st)
	if errors.Is(err, sql.ErrNoRows) {
		return Timer{}, false, nil
	}
	if err != nil {
		return Timer{}, false, err
	}
	t.FireAt = time.Unix(fa, 0).UTC()
	t.State = TimerState(st)
	return t, true, nil
}

// ClaimTimer atomically transitions a pending timer to fired. It returns false
// if another worker got there first — this is the exactly-once gate.
func (s *Store) ClaimTimer(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE timers SET state = 'fired' WHERE id = ? AND state = 'pending'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CancelTimers cancels every pending timer for an incident.
func (s *Store) CancelTimers(ctx context.Context, incidentID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE timers SET state = 'cancelled' WHERE incident_id = ? AND state = 'pending'`,
		incidentID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecordDelivery marks a step as delivered. It returns false when the step was
// already delivered, in which case the caller must not send again.
func (s *Store) RecordDelivery(ctx context.Context, incidentID int64, step int, target, responder string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO deliveries (incident_id, step, target, responder, at)
		 VALUES (?,?,?,?,?)`, incidentID, step, target, responder, unix(at))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetStep records how far up the ladder an incident has climbed.
func (s *Store) SetStep(ctx context.Context, incidentID int64, step int, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET step = ?, updated_at = ? WHERE id = ?`, step, unix(at), incidentID)
	return err
}

// Ack transitions an open incident to acked. It returns false if the incident
// was not open — a second ack, or an ack after resolve, is a no-op.
func (s *Store) Ack(ctx context.Context, incidentID int64, actor string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET status = 'acked', acked_at = ?, acked_by = ?, updated_at = ?
		 WHERE id = ? AND status IN ('open','held')`,
		unix(at), actor, unix(at), incidentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Resolve closes an incident.
func (s *Store) Resolve(ctx context.Context, incidentID int64, actor string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET status = 'resolved', resolved_at = ?, resolved_by = ?, updated_at = ?
		 WHERE id = ? AND status != 'resolved'`,
		unix(at), actor, unix(at), incidentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// AppendLedger writes one audit row.
func (s *Store) AppendLedger(ctx context.Context, incidentID int64, at time.Time, actor, action, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ledger (incident_id, at, actor, action, detail) VALUES (?,?,?,?,?)`,
		incidentID, unix(at), actor, action, detail)
	return err
}

// Ledger returns audit rows oldest first. incidentID 0 means all incidents.
func (s *Store) Ledger(ctx context.Context, incidentID int64) ([]LedgerEntry, error) {
	q := `SELECT id, incident_id, at, actor, action, detail FROM ledger`
	var args []any
	if incidentID != 0 {
		q += ` WHERE incident_id = ?`
		args = append(args, incidentID)
	}
	q += ` ORDER BY at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		var at int64
		if err := rows.Scan(&e.ID, &e.IncidentID, &at, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// LedgerBetween returns audit rows for every incident with since <= at < until,
// oldest first.
func (s *Store) LedgerBetween(ctx context.Context, since, until time.Time) ([]LedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, incident_id, at, actor, action, detail FROM ledger
		 WHERE at >= ? AND at < ? ORDER BY at, id`, unix(since), unix(until))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		var at int64
		if err := rows.Scan(&e.ID, &e.IncidentID, &at, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordFiring appends a firing event and returns how many firings for that
// fingerprint fall inside the window ending at now.
func (s *Store) RecordFiring(ctx context.Context, fingerprint string, now time.Time, window time.Duration) (int, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO firings (fingerprint, at) VALUES (?,?)`, fingerprint, unix(now)); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM firings WHERE fingerprint = ? AND at >= ?`,
		fingerprint, unix(now.Add(-window))).Scan(&n)
	return n, err
}

// Beat records a heartbeat check-in.
func (s *Store) Beat(ctx context.Context, name string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO heartbeats (name, last_seen, missed) VALUES (?,?,0)
		 ON CONFLICT(name) DO UPDATE SET last_seen = excluded.last_seen, missed = 0`,
		name, unix(at))
	return err
}

// LastBeat returns the last check-in for a heartbeat.
func (s *Store) LastBeat(ctx context.Context, name string) (time.Time, bool, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT last_seen FROM heartbeats WHERE name = ?`, name).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(at, 0).UTC(), true, nil
}

// MarkMissed flags a heartbeat as already paged, so it pages once per outage
// rather than on every check.
func (s *Store) MarkMissed(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE heartbeats SET missed = 1 WHERE name = ? AND missed = 0`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
