// Package rota resolves who is on call for a given rota at a given instant.
//
// Rotation is computed from calendar days in the rota's own timezone rather
// than from a fixed number of hours, so a DST transition does not slide the
// handover by an hour and does not hand the pager to the wrong person.
package rota

import (
	"fmt"
	"time"

	"github.com/moveeeax/telegram-oncall/internal/config"
)

// Assignment is the resolved on-call responder for a rota at some instant.
type Assignment struct {
	Rota         string
	Responder    config.Responder
	FromOverride bool
	Reason       string
	PeriodStart  time.Time
	PeriodEnd    time.Time
}

// Resolver resolves rota assignments against a config.
type Resolver struct{ cfg *config.Config }

// NewResolver returns a Resolver bound to cfg.
func NewResolver(cfg *config.Config) *Resolver { return &Resolver{cfg: cfg} }

// OnCall returns the responder carrying the named rota at time t.
func (r *Resolver) OnCall(rotaName string, t time.Time) (Assignment, error) {
	rt, ok := r.cfg.Rota(rotaName)
	if !ok {
		return Assignment{}, fmt.Errorf("rota: unknown rota %q", rotaName)
	}
	return r.resolve(rt, t)
}

func (r *Resolver) resolve(rt config.Rota, t time.Time) (Assignment, error) {
	// A manual override always wins, including over a handover that would have
	// happened inside the override window.
	for _, o := range rt.Overrides {
		if !t.Before(o.From) && t.Before(o.To) {
			resp, ok := r.cfg.Responder(o.Responder)
			if !ok {
				return Assignment{}, fmt.Errorf("rota %s: override names unknown responder %q", rt.Name, o.Responder)
			}
			reason := "manual override"
			if o.Reason != "" {
				reason = "manual override: " + o.Reason
			}
			return Assignment{
				Rota: rt.Name, Responder: resp, FromOverride: true, Reason: reason,
				PeriodStart: o.From.UTC(), PeriodEnd: o.To.UTC(),
			}, nil
		}
	}

	loc, err := time.LoadLocation(rt.Timezone)
	if err != nil {
		return Assignment{}, fmt.Errorf("rota %s: %w", rt.Name, err)
	}
	handover, err := time.Parse("15:04", rt.HandoverTime)
	if err != nil {
		return Assignment{}, fmt.Errorf("rota %s: bad handover_time %q", rt.Name, rt.HandoverTime)
	}
	startDay, err := time.ParseInLocation("2006-01-02", rt.StartDate, loc)
	if err != nil {
		return Assignment{}, fmt.Errorf("rota %s: bad start_date %q", rt.Name, rt.StartDate)
	}

	local := t.In(loc)
	// The "rota day" is the calendar day whose handover most recently passed.
	rotaDay := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	if local.Hour()*60+local.Minute() < handover.Hour()*60+handover.Minute() {
		rotaDay = rotaDay.AddDate(0, 0, -1)
	}

	period := 1
	if rt.Rotation == config.RotationWeekly {
		period = 7
	}
	days := daysBetween(startDay, rotaDay)
	idx := floorDiv(days, period)
	n := len(rt.Members)
	if n == 0 {
		return Assignment{}, fmt.Errorf("rota %s: has no members", rt.Name)
	}
	member := rt.Members[((idx%n)+n)%n]
	resp, ok := r.cfg.Responder(member)
	if !ok {
		return Assignment{}, fmt.Errorf("rota %s: member %q is not a declared responder", rt.Name, member)
	}

	periodStartDay := startDay.AddDate(0, 0, idx*period)
	ps := withTime(periodStartDay, handover, loc)
	pe := withTime(periodStartDay.AddDate(0, 0, period), handover, loc)

	return Assignment{
		Rota:      rt.Name,
		Responder: resp,
		Reason: fmt.Sprintf("%s rotation, period %d, member %d/%d",
			rt.Rotation, idx, ((idx%n)+n)%n+1, n),
		PeriodStart: ps.UTC(),
		PeriodEnd:   pe.UTC(),
	}, nil
}

// Schedule returns the next n assignments for a rota starting at t, which is
// what a read-only config viewer renders.
func (r *Resolver) Schedule(rotaName string, t time.Time, n int) ([]Assignment, error) {
	out := make([]Assignment, 0, n)
	cur := t
	for i := 0; i < n; i++ {
		a, err := r.OnCall(rotaName, cur)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
		if !a.PeriodEnd.After(cur) {
			break
		}
		cur = a.PeriodEnd
	}
	return out, nil
}

func withTime(day time.Time, hm time.Time, loc *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
}

// daysBetween counts whole calendar days from a to b, ignoring clock time.
func daysBetween(a, b time.Time) int {
	au := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	bu := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(bu.Sub(au).Hours() / 24)
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
