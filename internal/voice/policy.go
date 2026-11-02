package voice

import (
	"context"
	"time"

	"github.com/pod32g/omni-notify/internal/config"
	"github.com/pod32g/omni-notify/internal/models"
)

// busyRetry is how long a call waits when its recipient is already on a call.
const busyRetry = 30 * time.Second

// decision is the policy verdict for ringing a call now.
type decision struct {
	ring       bool
	deferUntil time.Time
	expiresAt  time.Time // set when a deferral moves the call's expiry too
	reason     string    // quiet_hours, busy, rate_limit, cooldown, expired
	expire     bool
}

// evaluate applies the recipient's policy to a call that is due now: expiry,
// quiet hours (unless an exception applies), one call at a time, the hourly
// rate limit and the per-agent cooldown. Quiet hours move the call's expiry
// along with it (the recipient's rule should not cost them the call); any
// other deferral past the call's expiry expires it instead.
func (s *Service) evaluate(ctx context.Context, c models.Call, u config.VoiceUser, a config.VoiceAgent, now time.Time) (decision, error) {
	if !now.Before(c.ExpiresAt) {
		return decision{expire: true, reason: "expired"}, nil
	}
	d, err := s.limits(ctx, c, u, a, now, true)
	if err != nil || d.ring {
		return d, err
	}
	if d.reason == "quiet_hours" {
		d.expiresAt = d.deferUntil.Add(c.ExpiresAt.Sub(c.ScheduledAt))
		return d, nil
	}
	if !d.deferUntil.Before(c.ExpiresAt) {
		return decision{expire: true, reason: d.reason}, nil
	}
	return d, nil
}

// limits checks everything but expiry. checkBusy is false when only asking
// whether a new immediate call would be allowed.
func (s *Service) limits(ctx context.Context, c models.Call, u config.VoiceUser, a config.VoiceAgent, now time.Time, checkBusy bool) (decision, error) {
	if end, quiet := quietUntil(u, now); quiet && !exempt(c, u, a) {
		return decision{deferUntil: end, reason: "quiet_hours"}, nil
	}
	if checkBusy {
		live, err := s.store.LiveCalls(ctx, c.Recipient)
		if err != nil {
			return decision{}, err
		}
		for _, l := range live {
			if l.ID != c.ID {
				return decision{deferUntil: now.Add(busyRetry), reason: "busy"}, nil
			}
		}
	}
	if max := u.RateLimit.MaxCallsPerHour; max > 0 {
		rang, err := s.store.RangSince(ctx, c.Recipient, "", now.Add(-time.Hour))
		if err != nil {
			return decision{}, err
		}
		if len(rang) >= max {
			return decision{deferUntil: rang[len(rang)-max].Add(time.Hour), reason: "rate_limit"}, nil
		}
	}
	if cd := u.RateLimit.Cooldown.D(); cd > 0 {
		rang, err := s.store.RangSince(ctx, c.Recipient, c.Agent, now.Add(-cd))
		if err != nil {
			return decision{}, err
		}
		if len(rang) > 0 {
			return decision{deferUntil: rang[len(rang)-1].Add(cd), reason: "cooldown"}, nil
		}
	}
	return decision{ring: true}, nil
}

// exempt reports whether a call may ring during quiet hours. Both sides must
// agree: the user allows the kind of exception, and the agent was granted it
// in config (an agent cannot claim it per request). Snoozes and reschedules
// the user spoke on a call (origin "user") are the user's own request.
func exempt(c models.Call, u config.VoiceUser, a config.VoiceAgent) bool {
	if c.UserRequested && u.Exceptions.ExplicitUserReminders &&
		(c.Origin == OriginUser || a.MayOverrideQuietHoursForUserReminders) {
		return true
	}
	return c.Severity == models.SeverityCritical && u.Exceptions.CriticalIncidents && a.MayOverrideQuietHoursWhenCritical
}

// quietUntil reports whether t falls in the user's quiet hours and, if so,
// when they end.
func quietUntil(u config.VoiceUser, t time.Time) (time.Time, bool) {
	q := u.QuietHours
	if q == nil {
		return time.Time{}, false
	}
	start, err1 := config.ParseClock(q.Start)
	end, err2 := config.ParseClock(q.End)
	if err1 != nil || err2 != nil || start == end {
		return time.Time{}, false
	}
	lt := t.In(u.Location())
	m := lt.Hour()*60 + lt.Minute()
	var quiet bool
	if start < end {
		quiet = m >= start && m < end
	} else {
		quiet = m >= start || m < end
	}
	if !quiet {
		return time.Time{}, false
	}
	until := time.Date(lt.Year(), lt.Month(), lt.Day(), end/60, end%60, 0, 0, u.Location())
	if !until.After(lt) {
		until = until.AddDate(0, 0, 1)
	}
	return until, true
}
