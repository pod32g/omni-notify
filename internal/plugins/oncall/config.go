package oncall

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// Config is the plugins.oncall section. The directory (people, teams,
// schedules, escalation policies, services) is defined here and re-read at
// every start; incidents, overrides and the activity feed live in the
// plugin's own SQLite file.
type Config struct {
	Enabled bool `yaml:"enabled"`
	Storage struct {
		// Path is the plugin's own database, separate from Notify's.
		Path string `yaml:"path"`
	} `yaml:"storage"`
	// EscalationDelay is how long a level has to acknowledge before the next
	// one is paged, when the level doesn't say.
	EscalationDelay    models.Duration  `yaml:"escalation_delay"`
	Users              []UserConfig     `yaml:"users"`
	Teams              []TeamConfig     `yaml:"teams"`
	Schedules          []ScheduleConfig `yaml:"schedules"`
	EscalationPolicies []PolicyConfig   `yaml:"escalation_policies"`
	Services           []ServiceConfig  `yaml:"services"`
}

// UserConfig is one person who can be on call.
type UserConfig struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Email    string `yaml:"email"`
	Timezone string `yaml:"timezone"`
	Phone    string `yaml:"phone"`
	// TokenSHA256 is the hex SHA-256 of the person's API token (until sign-in
	// with Omni-Identity). The token itself is never stored.
	TokenSHA256 string `yaml:"token_sha256"`
	// Contacts are the Notify providers a page to this person goes through.
	Contacts []string `yaml:"contacts"`
	// Capabilities default to everything but request_swaps.
	Capabilities []string `yaml:"capabilities"`
}

// TeamConfig is a team and its members.
type TeamConfig struct {
	ID      string   `yaml:"id"`
	Name    string   `yaml:"name"`
	Members []string `yaml:"members"`
}

// ScheduleConfig is a rotation: the users take turns of one day or one week,
// handing off at the same wall-clock time in the schedule's time zone.
type ScheduleConfig struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Team     string `yaml:"team"`
	Timezone string `yaml:"timezone"`
	Rotation string `yaml:"rotation"` // daily or weekly
	Handoff  string `yaml:"handoff"`  // "09:00"
	// Start is the date (YYYY-MM-DD) the first user's turn begins.
	Start string   `yaml:"start"`
	Users []string `yaml:"users"`
	// Role names the shift ("Primary"); by default it follows the level.
	Role string `yaml:"role"`
}

// PolicyConfig is an escalation policy: who is paged, and in what order.
type PolicyConfig struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
	Team string `yaml:"team"`
	// Repeat is how many times to start again from level 1 after the last.
	Repeat int           `yaml:"repeat"`
	Levels []LevelConfig `yaml:"levels"`
}

// LevelConfig is one escalation level.
type LevelConfig struct {
	Delay     models.Duration `yaml:"delay"`
	Schedules []string        `yaml:"schedules"`
	Users     []string        `yaml:"users"`
}

// ServiceConfig is something incidents are opened against.
type ServiceConfig struct {
	ID               string `yaml:"id"`
	Name             string `yaml:"name"`
	Description      string `yaml:"description"`
	Team             string `yaml:"team"`
	EscalationPolicy string `yaml:"escalation_policy"`
	// AutoResolve resolves an incident when its alert resolves (default true).
	AutoResolve *bool `yaml:"auto_resolve"`
}

var allCapabilities = []string{"create_incident", "reassign", "escalate", "resolve", "manage_overrides", "request_swaps"}

// ---- the directory built from it ----

// Ref is an id and a display name.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// User is a person as the API shows them.
type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Timezone string `json:"timezone"`
	Phone    string `json:"phone,omitempty"`
}

type person struct {
	User
	token    []byte // SHA-256, nil without one
	contacts []string
	caps     map[string]bool
	teams    []string
}

type team struct {
	Ref
	members []string
}

type schedule struct {
	Ref
	team     Ref
	tz       string
	loc      *time.Location
	rotation string
	handoff  string
	days     int
	anchor   time.Time // the first turn's start
	users    []string
	role     string
}

type level struct {
	delay     time.Duration
	schedules []string
	users     []string
}

type policy struct {
	Ref
	team   Ref
	repeat int
	levels []level
}

type service struct {
	Ref
	description string
	team        Ref
	policy      Ref
	autoResolve bool
}

type directory struct {
	users     map[string]*person
	teams     map[string]*team
	schedules map[string]*schedule
	policies  map[string]*policy
	services  map[string]*service
	// Config order, for stable listings.
	userIDs, scheduleIDs, policyIDs, serviceIDs []string
}

func (d *directory) user(id string) User {
	if p, ok := d.users[id]; ok {
		return p.User
	}
	return User{ID: id, Name: id}
}

func (d *directory) usersOf(ids []string) []User {
	out := make([]User, 0, len(ids))
	for _, id := range ids {
		out = append(out, d.user(id))
	}
	return out
}

func (d *directory) teamRef(id string) Ref {
	if t, ok := d.teams[id]; ok {
		return t.Ref
	}
	return Ref{ID: id, Name: id}
}

// buildDirectory checks the config and resolves every reference.
func buildDirectory(c Config) (*directory, error) {
	d := &directory{
		users: map[string]*person{}, teams: map[string]*team{}, schedules: map[string]*schedule{},
		policies: map[string]*policy{}, services: map[string]*service{},
	}
	defaultDelay := c.EscalationDelay.D()
	if defaultDelay <= 0 {
		defaultDelay = 5 * time.Minute
	}
	for _, u := range c.Users {
		if u.ID == "" || u.Name == "" {
			return nil, fmt.Errorf("users: every user needs an id and a name")
		}
		if _, dup := d.users[u.ID]; dup {
			return nil, fmt.Errorf("users: duplicate id %q", u.ID)
		}
		tz := u.Timezone
		if tz == "" {
			tz = "UTC"
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return nil, fmt.Errorf("users.%s: timezone: %w", u.ID, err)
		}
		p := &person{User: User{ID: u.ID, Name: u.Name, Email: u.Email, Timezone: tz, Phone: u.Phone},
			contacts: u.Contacts, caps: map[string]bool{}}
		if u.TokenSHA256 != "" {
			sum, err := hex.DecodeString(strings.ToLower(u.TokenSHA256))
			if err != nil || len(sum) != 32 {
				return nil, fmt.Errorf("users.%s: token_sha256 must be 64 hex characters", u.ID)
			}
			p.token = sum
		}
		caps := u.Capabilities
		if caps == nil {
			caps = allCapabilities[:len(allCapabilities)-1]
		}
		for _, c := range caps {
			if !contains(allCapabilities, c) {
				return nil, fmt.Errorf("users.%s: unknown capability %q", u.ID, c)
			}
			p.caps[c] = true
		}
		d.users[u.ID] = p
		d.userIDs = append(d.userIDs, u.ID)
	}
	knownUsers := func(where string, ids []string) error {
		for _, id := range ids {
			if _, ok := d.users[id]; !ok {
				return fmt.Errorf("%s: unknown user %q", where, id)
			}
		}
		return nil
	}
	for _, t := range c.Teams {
		if t.ID == "" || t.Name == "" {
			return nil, fmt.Errorf("teams: every team needs an id and a name")
		}
		if err := knownUsers("teams."+t.ID, t.Members); err != nil {
			return nil, err
		}
		d.teams[t.ID] = &team{Ref: Ref{t.ID, t.Name}, members: t.Members}
		for _, m := range t.Members {
			d.users[m].teams = append(d.users[m].teams, t.ID)
		}
	}
	knownTeam := func(where, id string) error {
		if _, ok := d.teams[id]; id != "" && !ok {
			return fmt.Errorf("%s: unknown team %q", where, id)
		}
		return nil
	}
	for _, s := range c.Schedules {
		where := "schedules." + s.ID
		if s.ID == "" || s.Name == "" || len(s.Users) == 0 {
			return nil, fmt.Errorf("schedules: every schedule needs an id, a name and users")
		}
		if err := knownUsers(where, s.Users); err != nil {
			return nil, err
		}
		if err := knownTeam(where, s.Team); err != nil {
			return nil, err
		}
		tz := s.Timezone
		if tz == "" {
			tz = "UTC"
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("%s: timezone: %w", where, err)
		}
		days := map[string]int{"daily": 1, "weekly": 7}[s.Rotation]
		if days == 0 {
			return nil, fmt.Errorf("%s: rotation must be daily or weekly", where)
		}
		handoff := s.Handoff
		if handoff == "" {
			handoff = "09:00"
		}
		hm, err := time.Parse("15:04", handoff)
		if err != nil {
			return nil, fmt.Errorf("%s: handoff must be HH:MM", where)
		}
		day, err := time.ParseInLocation("2006-01-02", s.Start, loc)
		if err != nil {
			return nil, fmt.Errorf("%s: start must be YYYY-MM-DD", where)
		}
		anchor := time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
		d.schedules[s.ID] = &schedule{Ref: Ref{s.ID, s.Name}, team: d.teamRef(s.Team), tz: tz, loc: loc,
			rotation: s.Rotation, handoff: handoff, days: days, anchor: anchor, users: s.Users, role: s.Role}
		d.scheduleIDs = append(d.scheduleIDs, s.ID)
	}
	for _, p := range c.EscalationPolicies {
		where := "escalation_policies." + p.ID
		if p.ID == "" || p.Name == "" || len(p.Levels) == 0 {
			return nil, fmt.Errorf("escalation_policies: every policy needs an id, a name and levels")
		}
		if err := knownTeam(where, p.Team); err != nil {
			return nil, err
		}
		pol := &policy{Ref: Ref{p.ID, p.Name}, team: d.teamRef(p.Team), repeat: max(0, p.Repeat)}
		for i, l := range p.Levels {
			lw := fmt.Sprintf("%s.levels[%d]", where, i)
			if len(l.Schedules)+len(l.Users) == 0 {
				return nil, fmt.Errorf("%s: a level needs schedules or users", lw)
			}
			for _, id := range l.Schedules {
				if _, ok := d.schedules[id]; !ok {
					return nil, fmt.Errorf("%s: unknown schedule %q", lw, id)
				}
			}
			if err := knownUsers(lw, l.Users); err != nil {
				return nil, err
			}
			delay := l.Delay.D()
			if delay <= 0 {
				delay = defaultDelay
			}
			pol.levels = append(pol.levels, level{delay: delay, schedules: l.Schedules, users: l.Users})
		}
		d.policies[p.ID] = pol
		d.policyIDs = append(d.policyIDs, p.ID)
	}
	for _, s := range c.Services {
		where := "services." + s.ID
		if s.ID == "" || s.Name == "" {
			return nil, fmt.Errorf("services: every service needs an id and a name")
		}
		pol, ok := d.policies[s.EscalationPolicy]
		if !ok {
			return nil, fmt.Errorf("%s: unknown escalation_policy %q", where, s.EscalationPolicy)
		}
		if err := knownTeam(where, s.Team); err != nil {
			return nil, err
		}
		teamRef := pol.team
		if s.Team != "" {
			teamRef = d.teamRef(s.Team)
		}
		d.services[s.ID] = &service{Ref: Ref{s.ID, s.Name}, description: s.Description, team: teamRef,
			policy: pol.Ref, autoResolve: s.AutoResolve == nil || *s.AutoResolve}
		d.serviceIDs = append(d.serviceIDs, s.ID)
	}
	return d, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
