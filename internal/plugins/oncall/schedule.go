package oncall

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Shift is someone on call, as the API shows it.
type Shift struct {
	User             User      `json:"user"`
	EscalationPolicy Ref       `json:"escalation_policy"`
	Level            int       `json:"level"`
	Schedule         *Ref      `json:"schedule"`
	Role             string    `json:"role"`
	Start            time.Time `json:"start"`
	End              time.Time `json:"end"`
	IsOverride       bool      `json:"is_override"`
}

// override is someone covering part of a schedule.
type override struct {
	ID         string    `json:"id"`
	ScheduleID string    `json:"schedule_id"`
	UserID     string    `json:"user_id"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// span is one stretch of a schedule with one person on it.
type span struct {
	user       string
	start, end time.Time
	override   bool
}

// turnStart is the start of turn k. AddDate keeps the wall-clock handoff time,
// so a turn that crosses a daylight-saving change is an hour longer or shorter.
func (sc *schedule) turnStart(k int) time.Time {
	return sc.anchor.AddDate(0, 0, k*sc.days)
}

// turnAt is the turn that covers t.
func (sc *schedule) turnAt(t time.Time) int {
	k := int(math.Floor(t.Sub(sc.anchor).Hours() / 24 / float64(sc.days)))
	for sc.turnStart(k).After(t) {
		k--
	}
	for !sc.turnStart(k + 1).After(t) {
		k++
	}
	return k
}

func (sc *schedule) userFor(k int) string {
	n := len(sc.users)
	return sc.users[((k%n)+n)%n]
}

// spans is who covers sc in [since, until): the rotation with overrides laid
// over it, later overrides over earlier ones.
func spans(sc *schedule, overrides []override, since, until time.Time) []span {
	var out []span
	for k := sc.turnAt(since); sc.turnStart(k).Before(until); k++ {
		out = append(out, span{user: sc.userFor(k), start: sc.turnStart(k), end: sc.turnStart(k + 1)})
	}
	for _, o := range overrides {
		if !o.End.After(since) || !o.Start.Before(until) {
			continue
		}
		out = carve(out, o.Start, o.End)
		out = append(out, span{user: o.UserID, start: o.Start, end: o.End, override: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

// carve removes [start, end) from every span.
func carve(in []span, start, end time.Time) []span {
	out := in[:0:0]
	for _, s := range in {
		if !s.start.Before(end) || !s.end.After(start) {
			out = append(out, s)
			continue
		}
		if s.start.Before(start) {
			out = append(out, span{user: s.user, start: s.start, end: start, override: s.override})
		}
		if s.end.After(end) {
			out = append(out, span{user: s.user, start: end, end: s.end, override: s.override})
		}
	}
	return out
}

// onCallAt is the span covering t.
func onCallAt(sc *schedule, overrides []override, t time.Time) span {
	for _, s := range spans(sc, overrides, t, t.Add(time.Nanosecond)) {
		if !t.Before(s.start) && t.Before(s.end) {
			return s
		}
	}
	return span{}
}

// placement is where a schedule sits in the escalation policies: the first
// policy level that pages it.
func (d *directory) placement(scheduleID string) (Ref, int) {
	for _, pid := range d.policyIDs {
		for i, l := range d.policies[pid].levels {
			if contains(l.schedules, scheduleID) {
				return d.policies[pid].Ref, i + 1
			}
		}
	}
	return Ref{}, 0
}

func roleFor(sc *schedule, level int) string {
	switch {
	case sc != nil && sc.role != "":
		return sc.role
	case level == 1:
		return "Primary"
	case level == 2:
		return "Secondary"
	default:
		return fmt.Sprintf("Level %d", level)
	}
}

func (d *directory) shift(sc *schedule, s span, pol Ref, lvl int) Shift {
	ref := sc.Ref
	return Shift{User: d.user(s.user), EscalationPolicy: pol, Level: lvl, Schedule: &ref, Role: roleFor(sc, lvl),
		Start: s.start, End: s.end, IsOverride: s.override}
}

// levelUsers is everyone a policy level pages at t: who is on its schedules,
// then its named users, once each.
func (p *Plugin) levelUsers(pol *policy, lvl int, t time.Time) []string {
	if lvl < 1 || lvl > len(pol.levels) {
		return nil
	}
	l := pol.levels[lvl-1]
	var out []string
	for _, sid := range l.schedules {
		if s := onCallAt(p.dir.schedules[sid], p.overrides[sid], t); s.user != "" && !contains(out, s.user) {
			out = append(out, s.user)
		}
	}
	for _, u := range l.users {
		if !contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}
