package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// VoiceConfig enables the VOICE_CALL channel: who may place calls (agents),
// who may receive them (users, with their policy), and how the phone is rung.
// Disabled by default; nothing changes for existing deployments until a voice
// section is added.
type VoiceConfig struct {
	Enabled bool `yaml:"enabled"`
	// RingTimeout is how long the server lets a call ring before it is MISSED.
	RingTimeout models.Duration `yaml:"ring_timeout"`
	// DeviceRingTimeout is how long the phone shows the incoming call; shorter
	// than RingTimeout so the device, not the sweeper, normally reports a miss.
	DeviceRingTimeout models.Duration `yaml:"device_ring_timeout"`
	// ActiveTimeout fails an answered call with no device activity for this long.
	ActiveTimeout models.Duration `yaml:"active_timeout"`
	// DefaultExpiry bounds how long after its scheduled time a call may still
	// ring (after quiet-hour deferrals or retries).
	DefaultExpiry models.Duration `yaml:"default_expiry"`
	// MaxScheduleAhead bounds how far in the future a call may be scheduled.
	MaxScheduleAhead models.Duration `yaml:"max_schedule_ahead"`
	// EnrollmentTTL is how long a device enrollment code stays valid.
	EnrollmentTTL models.Duration `yaml:"enrollment_ttl"`
	// StoreTranscripts keeps what the user said on calls. Off by default:
	// only the interpreted intent is stored.
	StoreTranscripts bool `yaml:"store_transcripts"`

	APNS     APNSConfig     `yaml:"apns"`
	Identity IdentityConfig `yaml:"identity"`
	Agents   []VoiceAgent   `yaml:"agents"`
	Users    []VoiceUser    `yaml:"users"`
}

// APNSConfig holds the token-based (.p8) APNs credentials used for VoIP pushes.
// When neither KeyID nor Provider is set no push is sent and devices must poll
// (development).
type APNSConfig struct {
	// Provider names a Notify provider of kind apns (the on-call app's push)
	// whose key, key id, team and topic to use, so the .p8 stays in one place,
	// encrypted in Notify's database. Instead of the fields below.
	Provider string `yaml:"provider"`
	KeyID    string `yaml:"key_id"`
	TeamID   string `yaml:"team_id"`
	KeyPath  string `yaml:"key_path"` // PEM .p8 file
	Key      string `yaml:"key"`      // or the PEM itself (${ENV})
	// Topic is the app's bundle id; ".voip" is appended for VoIP pushes.
	Topic string `yaml:"topic"`
	// DevelopmentURL / ProductionURL override the APNs hosts (tests).
	DevelopmentURL string `yaml:"development_url"`
	ProductionURL  string `yaml:"production_url"`
}

// Configured reports whether VoIP pushes can be sent.
func (a APNSConfig) Configured() bool { return a.KeyID != "" || a.Provider != "" }

// IdentityConfig points at the Omni-Identity issuer whose client_credentials
// tokens identify agents.
type IdentityConfig struct {
	Issuer   string          `yaml:"issuer"`
	JWKSURL  string          `yaml:"jwks_url"`
	CacheTTL models.Duration `yaml:"cache_ttl"`
}

// Configured reports whether Identity tokens are accepted.
func (i IdentityConfig) Configured() bool { return i.Issuer != "" && i.JWKSURL != "" }

// VoiceAgent is a principal allowed to place calls. Its identity comes from
// authentication (an Identity client or a static token), never from the request.
type VoiceAgent struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
	// IdentityClients are Omni-Identity client ids whose client_credentials
	// tokens authenticate as this agent.
	IdentityClients []string `yaml:"identity_clients"`
	// TokenSHA256 are hex SHA-256 digests of static bearer tokens for this
	// agent (a trusted service mapping for local or test use).
	TokenSHA256 []string `yaml:"token_sha256"`
	// MayOverrideQuietHoursForUserReminders lets this agent's user-requested
	// reminders ring during quiet hours (if the user also allows that).
	MayOverrideQuietHoursForUserReminders bool `yaml:"may_override_quiet_hours_for_user_reminders"`
	// MayOverrideQuietHoursWhenCritical lets this agent's critical calls ring
	// during quiet hours (if the user also allows that).
	MayOverrideQuietHoursWhenCritical bool `yaml:"may_override_quiet_hours_when_critical"`
	// AllowTest lets the agent place calls marked as tests.
	AllowTest bool `yaml:"allow_test"`
	// Actions the recipient may take on this agent's calls. Default: all of
	// acknowledge, dismiss, snooze, reschedule.
	Actions []string `yaml:"actions"`
	// MaxPending caps this agent's outstanding scheduled calls (default 20).
	MaxPending int `yaml:"max_pending"`
}

// VoiceUser is a call recipient and the policy that governs calls to them.
type VoiceUser struct {
	ID       string   `yaml:"id"`
	Aliases  []string `yaml:"aliases"`
	Timezone string   `yaml:"timezone"`
	// AllowedAgents may call this user; empty means nobody.
	AllowedAgents []string    `yaml:"allowed_agents"`
	QuietHours    *QuietHours `yaml:"quiet_hours"`
	Exceptions    struct {
		ExplicitUserReminders bool `yaml:"explicit_user_reminders"`
		CriticalIncidents     bool `yaml:"critical_incidents"`
	} `yaml:"exceptions"`
	RateLimit struct {
		MaxCallsPerHour int             `yaml:"max_calls_per_hour"`
		Cooldown        models.Duration `yaml:"cooldown"`
	} `yaml:"rate_limit"`
	Retry struct {
		MaxAttempts int             `yaml:"max_attempts"`
		Delay       models.Duration `yaml:"delay"`
	} `yaml:"retry"`
	// MaxSnooze bounds how far a spoken snooze or reschedule may move a call.
	MaxSnooze models.Duration `yaml:"max_snooze"`
	// FallbackProvider (any configured provider) receives the notification
	// when its call ends missed, failed or expired.
	FallbackProvider string `yaml:"fallback_provider"`

	loc *time.Location
}

// Location returns the user's time zone (UTC if unset).
func (u VoiceUser) Location() *time.Location {
	if u.loc != nil {
		return u.loc
	}
	return time.UTC
}

// QuietHours is a daily window, in the user's time zone, during which calls
// wait unless an exception applies. Start may be later than End (overnight).
type QuietHours struct {
	Start string `yaml:"start"` // "HH:MM"
	End   string `yaml:"end"`
}

// DefaultCallActions are the in-call actions available when an agent lists none.
var DefaultCallActions = []string{"acknowledge", "dismiss", "snooze", "reschedule"}

func (v *VoiceConfig) applyDefaults() {
	if !v.Enabled {
		return
	}
	def := func(d *models.Duration, val time.Duration) {
		if *d == 0 {
			*d = models.Duration(val)
		}
	}
	def(&v.RingTimeout, 45*time.Second)
	def(&v.DeviceRingTimeout, 30*time.Second)
	def(&v.ActiveTimeout, 5*time.Minute)
	def(&v.DefaultExpiry, 2*time.Hour)
	def(&v.MaxScheduleAhead, 30*24*time.Hour)
	def(&v.EnrollmentTTL, 15*time.Minute)
	def(&v.Identity.CacheTTL, 10*time.Minute)
	for i := range v.Agents {
		a := &v.Agents[i]
		if a.DisplayName == "" {
			a.DisplayName = a.ID
		}
		if len(a.Actions) == 0 {
			a.Actions = append([]string(nil), DefaultCallActions...)
		}
		if a.MaxPending == 0 {
			a.MaxPending = 20
		}
	}
	for i := range v.Users {
		u := &v.Users[i]
		if u.RateLimit.MaxCallsPerHour == 0 {
			u.RateLimit.MaxCallsPerHour = 5
		}
		if u.Retry.MaxAttempts == 0 {
			u.Retry.MaxAttempts = 1
		}
		def(&u.Retry.Delay, 2*time.Minute)
		def(&u.MaxSnooze, 12*time.Hour)
	}
}

func (v *VoiceConfig) validate() error {
	if !v.Enabled {
		return nil
	}
	if v.DeviceRingTimeout > v.RingTimeout {
		return fmt.Errorf("voice.device_ring_timeout must not exceed voice.ring_timeout")
	}
	if v.APNS.Provider != "" && (v.APNS.KeyID != "" || v.APNS.KeyPath != "" || v.APNS.Key != "") {
		return fmt.Errorf("voice.apns: use provider or key_id/key, not both")
	}
	if v.APNS.KeyID != "" {
		if v.APNS.TeamID == "" || v.APNS.Topic == "" {
			return fmt.Errorf("voice.apns: team_id and topic are required with key_id")
		}
		if v.APNS.KeyPath == "" && v.APNS.Key == "" {
			return fmt.Errorf("voice.apns: key_path or key is required with key_id")
		}
	}
	if (v.Identity.Issuer == "") != (v.Identity.JWKSURL == "") {
		return fmt.Errorf("voice.identity: issuer and jwks_url go together")
	}
	agents := map[string]bool{}
	clients := map[string]string{}
	for _, a := range v.Agents {
		if a.ID == "" {
			return fmt.Errorf("voice.agents: id is required")
		}
		if agents[a.ID] {
			return fmt.Errorf("voice.agents: duplicate id %q", a.ID)
		}
		agents[a.ID] = true
		for _, c := range a.IdentityClients {
			if other, ok := clients[c]; ok {
				return fmt.Errorf("voice.agents: identity client %q maps to both %q and %q", c, other, a.ID)
			}
			clients[c] = a.ID
		}
		for _, h := range a.TokenSHA256 {
			if len(h) != 64 || strings.Trim(strings.ToLower(h), "0123456789abcdef") != "" {
				return fmt.Errorf("voice.agents[%s]: token_sha256 must be 64 hex characters (unresolved ${ENV}?)", a.ID)
			}
		}
		for _, act := range a.Actions {
			if !contains(DefaultCallActions, act) {
				return fmt.Errorf("voice.agents[%s]: unknown action %q", a.ID, act)
			}
		}
	}
	names := map[string]string{}
	for i := range v.Users {
		u := &v.Users[i]
		if u.ID == "" {
			return fmt.Errorf("voice.users: id is required")
		}
		for _, n := range append([]string{u.ID}, u.Aliases...) {
			if other, ok := names[n]; ok {
				return fmt.Errorf("voice.users: name %q used by both %q and %q", n, other, u.ID)
			}
			names[n] = u.ID
		}
		loc, err := time.LoadLocation(orDefault(u.Timezone, "UTC"))
		if err != nil {
			return fmt.Errorf("voice.users[%s]: timezone: %w", u.ID, err)
		}
		u.loc = loc
		for _, a := range u.AllowedAgents {
			if !agents[a] {
				return fmt.Errorf("voice.users[%s]: allowed agent %q is not in voice.agents", u.ID, a)
			}
		}
		if q := u.QuietHours; q != nil {
			if _, err := ParseClock(q.Start); err != nil {
				return fmt.Errorf("voice.users[%s].quiet_hours.start: %w", u.ID, err)
			}
			if _, err := ParseClock(q.End); err != nil {
				return fmt.Errorf("voice.users[%s].quiet_hours.end: %w", u.ID, err)
			}
		}
	}
	return nil
}

// ParseClock parses "HH:MM" into minutes after midnight.
func ParseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Agent returns the agent with the given id.
func (v VoiceConfig) Agent(id string) (VoiceAgent, bool) {
	for _, a := range v.Agents {
		if a.ID == id {
			return a, true
		}
	}
	return VoiceAgent{}, false
}

// AgentForClient returns the agent an Identity client id authenticates as.
func (v VoiceConfig) AgentForClient(clientID string) (VoiceAgent, bool) {
	for _, a := range v.Agents {
		if contains(a.IdentityClients, clientID) {
			return a, true
		}
	}
	return VoiceAgent{}, false
}

// AgentForTokenHash returns the agent a static token digest belongs to.
func (v VoiceConfig) AgentForTokenHash(hexDigest string) (VoiceAgent, bool) {
	for _, a := range v.Agents {
		for _, h := range a.TokenSHA256 {
			if strings.EqualFold(h, hexDigest) {
				return a, true
			}
		}
	}
	return VoiceAgent{}, false
}

// User resolves a recipient name (id or alias) to its user.
func (v VoiceConfig) User(name string) (VoiceUser, bool) {
	for _, u := range v.Users {
		if u.ID == name || contains(u.Aliases, name) {
			return u, true
		}
	}
	return VoiceUser{}, false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
