package oncall

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// pageSource is the Source of the events the plugin pages people with. An
// event from it routed back into the plugin is ignored, so a route can't loop.
const pageSource = "omni-oncall"

type source struct {
	Kind        string `json:"kind"`
	Ref         string `json:"ref"`
	Fingerprint string `json:"fingerprint"`
}

// incident is the stored incident. People are ids; render shows them.
type incident struct {
	ID          string     `json:"id"`
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Severity    string     `json:"severity"`
	Status      string     `json:"status"`
	ServiceID   string     `json:"service_id"`
	TeamID      string     `json:"team_id"`
	PolicyID    string     `json:"policy_id"`
	Level       int        `json:"level"`
	LoopsLeft   int        `json:"loops_left"`
	EscalatesAt *time.Time `json:"escalates_at,omitempty"`
	Assignees   []string   `json:"assignees"`
	AckedBy     []string   `json:"acknowledged_by"`
	Paged       []string   `json:"paged"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	AckedAt     *time.Time `json:"acknowledged_at,omitempty"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy  string     `json:"resolved_by,omitempty"`
	Source      source     `json:"source"`
	AlertCount  int        `json:"alert_count"`
	NoteCount   int        `json:"note_count"`
	Version     int        `json:"version"`
	Changed     int64      `json:"changed"`
}

func (inc *incident) open() bool { return inc.Status != "resolved" }

// event is one thing that happened: on an incident's timeline, in the feed,
// or both. Summaries are written when it is shown, for the person looking.
type event struct {
	Seq        int64     `json:"-"`
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	IncidentID string    `json:"incident_id,omitempty"`
	ScheduleID string    `json:"schedule_id,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	Source     string    `json:"source,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Channel    string    `json:"channel,omitempty"`
}

// Incident is an incident as the API shows it.
type Incident struct {
	ID               string     `json:"id"`
	Number           int        `json:"number"`
	Title            string     `json:"title"`
	Description      string     `json:"description"`
	Severity         string     `json:"severity"`
	Status           string     `json:"status"`
	Service          Ref        `json:"service"`
	Team             Ref        `json:"team"`
	EscalationPolicy Ref        `json:"escalation_policy"`
	EscalationLevel  int        `json:"escalation_level"`
	EscalatesAt      *time.Time `json:"escalates_at"`
	NextResponders   []User     `json:"next_responders"`
	Assignees        []User     `json:"assignees"`
	AcknowledgedBy   []User     `json:"acknowledged_by"`
	Responders       []User     `json:"responders"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	AcknowledgedAt   *time.Time `json:"acknowledged_at"`
	ResolvedAt       *time.Time `json:"resolved_at"`
	ResolvedBy       *User      `json:"resolved_by"`
	Source           source     `json:"source"`
	AlertCount       int        `json:"alert_count"`
	NoteCount        int        `json:"note_count"`
	Version          int        `json:"version"`
}

// Entry is a timeline entry as the API shows it.
type Entry struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Actor   *User     `json:"actor"`
	Summary string    `json:"summary"`
	Detail  string    `json:"detail"`
	Channel *string   `json:"channel"`
}

func (p *Plugin) serviceRef(id string) Ref {
	if s, ok := p.dir.services[id]; ok {
		return s.Ref
	}
	return Ref{ID: id, Name: id}
}

func (p *Plugin) policyRef(id string) Ref {
	if pol, ok := p.dir.policies[id]; ok {
		return pol.Ref
	}
	return Ref{ID: id, Name: id}
}

func (p *Plugin) render(inc *incident, now time.Time) Incident {
	out := Incident{
		ID: inc.ID, Number: inc.Number, Title: inc.Title, Description: inc.Description, Severity: inc.Severity,
		Status: inc.Status, Service: p.serviceRef(inc.ServiceID), Team: p.dir.teamRef(inc.TeamID),
		EscalationPolicy: p.policyRef(inc.PolicyID), EscalationLevel: inc.Level, EscalatesAt: inc.EscalatesAt,
		NextResponders: []User{}, Assignees: p.dir.usersOf(inc.Assignees), AcknowledgedBy: p.dir.usersOf(inc.AckedBy),
		Responders: p.dir.usersOf(inc.Paged), CreatedAt: inc.CreatedAt, UpdatedAt: inc.UpdatedAt,
		AcknowledgedAt: inc.AckedAt, ResolvedAt: inc.ResolvedAt, Source: inc.Source, AlertCount: inc.AlertCount,
		NoteCount: inc.NoteCount, Version: inc.Version,
	}
	if inc.ResolvedBy != "" {
		u := p.dir.user(inc.ResolvedBy)
		out.ResolvedBy = &u
	}
	if inc.Status == "triggered" {
		if lvl, ok, _ := p.next(inc); ok {
			out.NextResponders = p.dir.usersOf(p.levelUsers(p.dir.policies[inc.PolicyID], lvl, now))
		}
	}
	return out
}

// name is how a person reads in a sentence for the viewer.
func (p *Plugin) name(id, viewer string) string {
	if id == viewer {
		return "you"
	}
	return p.dir.user(id).Name
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (p *Plugin) summary(e event, viewer string) string {
	switch e.Kind {
	case "triggered":
		if e.Actor != "" {
			return "Opened by " + p.name(e.Actor, viewer)
		}
		return "Triggered by " + e.Source
	case "alert":
		return "The alert fired again"
	case "assigned":
		return "Assigned to " + p.name(e.Subject, viewer)
	case "notified":
		return "Notified " + p.name(e.Subject, viewer)
	case "acknowledged":
		return capitalize(p.name(e.Actor, viewer)) + " acknowledged"
	case "escalated":
		return "Escalated to " + p.name(e.Subject, viewer)
	case "reassigned":
		return "Reassigned to " + p.name(e.Subject, viewer)
	case "resolved":
		if e.Actor != "" {
			return capitalize(p.name(e.Actor, viewer)) + " resolved"
		}
		return "Resolved by " + e.Source
	case "note":
		return capitalize(p.name(e.Actor, viewer)) + " added a note"
	case "handoff":
		if e.Subject == viewer {
			return "You're on call for " + p.scheduleName(e.ScheduleID)
		}
		return p.dir.user(e.Subject).Name + " is on call for " + p.scheduleName(e.ScheduleID)
	case "override":
		if e.Subject == viewer {
			return "You cover " + p.scheduleName(e.ScheduleID)
		}
		return p.dir.user(e.Subject).Name + " covers " + p.scheduleName(e.ScheduleID)
	}
	return e.Kind
}

func (p *Plugin) scheduleName(id string) string {
	if sc, ok := p.dir.schedules[id]; ok {
		return sc.Name
	}
	return id
}

func (p *Plugin) entry(e event, viewer string) Entry {
	out := Entry{ID: "e_" + itoa(e.Seq), At: e.At, Kind: e.Kind, Summary: p.summary(e, viewer), Detail: e.Detail}
	if e.Actor != "" {
		u := p.dir.user(e.Actor)
		out.Actor = &u
	}
	if e.Channel != "" {
		c := e.Channel
		out.Channel = &c
	}
	return out
}

// record adds an event to an incident's timeline.
func (p *Plugin) record(ctx context.Context, inc *incident, e event) error {
	e.IncidentID = inc.ID
	if err := p.db.addEvent(ctx, &e); err != nil {
		return err
	}
	if e.At.After(inc.UpdatedAt) {
		inc.UpdatedAt = e.At
	}
	return nil
}

// touch saves a changed incident with a new version and change sequence.
func (p *Plugin) touch(ctx context.Context, inc *incident) error {
	inc.Version++
	p.changeSeq++
	inc.Changed = p.changeSeq
	if err := p.db.putIncident(ctx, inc); err != nil {
		return err
	}
	return p.saveCounters(ctx)
}

type counters struct {
	NextNumber int   `json:"next_number"`
	ChangeSeq  int64 `json:"change_seq"`
	OnCallSeq  int64 `json:"oncall_seq"`
}

func (p *Plugin) saveCounters(ctx context.Context) error {
	return p.db.setMeta(ctx, "counters", counters{p.nextNumber, p.changeSeq, p.oncallSeq})
}

// next is the level an unacknowledged incident escalates to, and whether that
// starts the policy over.
func (p *Plugin) next(inc *incident) (lvl int, ok, wraps bool) {
	pol := p.dir.policies[inc.PolicyID]
	switch {
	case pol == nil:
		return 0, false, false
	case inc.Level < len(pol.levels):
		return inc.Level + 1, true, false
	case inc.LoopsLeft > 0:
		return 1, true, true
	}
	return 0, false, false
}

// openIncident opens an incident on a service and pages its first level.
func (p *Plugin) openIncident(ctx context.Context, now time.Time, svc *service, title, desc, sev string, src source, actor string) (*incident, error) {
	n := p.nextNumber
	if n < 1 {
		n = 1
	}
	p.nextNumber = n + 1
	pol := p.dir.policies[svc.policy.ID]
	inc := &incident{
		ID: fmt.Sprintf("inc_%d", n), Number: n, Title: title, Description: desc, Severity: sev, Status: "triggered",
		ServiceID: svc.ID, TeamID: svc.team.ID, PolicyID: svc.policy.ID, LoopsLeft: pol.repeat,
		Assignees: []string{}, AckedBy: []string{}, Paged: []string{}, CreatedAt: now, UpdatedAt: now, Source: src,
	}
	if actor == "" {
		inc.AlertCount = 1
	}
	p.incidents[inc.ID] = inc
	detail := firstSentence(desc, title)
	if src.Ref != "" && actor == "" {
		detail = src.Ref + ": " + detail
	}
	if actor != "" {
		detail = "Opened by hand"
	}
	if err := p.record(ctx, inc, event{At: now, Kind: "triggered", Actor: actor, Source: src.Kind, Detail: detail}); err != nil {
		return nil, err
	}
	if err := p.assign(ctx, inc, 1, now, "assigned", ""); err != nil {
		return nil, err
	}
	return inc, p.touch(ctx, inc)
}

func firstSentence(desc, title string) string {
	if desc == "" {
		return title
	}
	if i := strings.Index(desc, ". "); i > 0 {
		return desc[:i]
	}
	return strings.TrimSuffix(desc, ".")
}

// assign gives the incident to a level of its policy, records why, pages
// everyone on that level and sets when it escalates next.
func (p *Plugin) assign(ctx context.Context, inc *incident, lvl int, now time.Time, kind, actor string) error {
	pol := p.dir.policies[inc.PolicyID]
	inc.Level = lvl
	inc.EscalatesAt = nil
	if pol == nil {
		inc.Assignees = []string{}
		return p.record(ctx, inc, event{At: now, Kind: kind, Actor: actor, Detail: "The escalation policy no longer exists"})
	}
	users := p.levelUsers(pol, lvl, now)
	inc.Assignees = append([]string{}, users...)
	if _, ok, _ := p.next(inc); ok {
		t := now.Add(pol.levels[lvl-1].delay)
		inc.EscalatesAt = &t
	}
	detail := fmt.Sprintf("%s · level %d", pol.Name, lvl)
	if kind == "escalated" && actor == "" {
		detail = fmt.Sprintf("No acknowledgement · level %d", lvl)
	}
	if len(users) == 0 {
		if err := p.record(ctx, inc, event{At: now, Kind: kind, Actor: actor, Detail: fmt.Sprintf("Nobody is on call at level %d", lvl)}); err != nil {
			return err
		}
	}
	for _, u := range users {
		if err := p.record(ctx, inc, event{At: now, Kind: kind, Actor: actor, Subject: u, Detail: detail}); err != nil {
			return err
		}
		if err := p.page(ctx, inc, u, now); err != nil {
			return err
		}
	}
	return nil
}

// page asks Notify to deliver a page to one person through their contacts.
func (p *Plugin) page(ctx context.Context, inc *incident, userID string, now time.Time) error {
	if !contains(inc.Paged, userID) {
		inc.Paged = append(inc.Paged, userID)
	}
	per := p.dir.users[userID]
	if per == nil || len(per.contacts) == 0 {
		return p.record(ctx, inc, event{At: now, Kind: "notified", Subject: userID, Detail: "Not paged: no contact methods"})
	}
	svc := p.serviceRef(inc.ServiceID)
	summary := fmt.Sprintf("#%d · level %d.", inc.Number, inc.Level)
	if inc.EscalatesAt != nil {
		summary += fmt.Sprintf(" Escalates in %d min unless acknowledged.", int(inc.EscalatesAt.Sub(now).Round(time.Minute).Minutes()))
	}
	ev := models.Event{
		EventID:     fmt.Sprintf("%s-%s-%d", inc.ID, userID, now.UnixNano()),
		Type:        "oncall.page",
		Source:      pageSource,
		Severity:    notifySeverity(inc.Severity),
		Title:       fmt.Sprintf("%s · %s: %s", strings.ToUpper(inc.Severity), svc.Name, inc.Title),
		Summary:     summary,
		Description: inc.Description,
		Labels: map[string]string{
			"oncall_incident": inc.ID, "oncall_user": userID, "service": svc.Name, "oncall_severity": inc.Severity,
		},
		Annotations: map[string]string{
			"incident_number": fmt.Sprint(inc.Number), "incident_version": fmt.Sprint(inc.Version + 1),
			"link": "omnioncall://incident/" + inc.ID,
		},
		Timestamp: now,
	}
	channel := strings.Join(per.contacts, ", ")
	detail := "Sent through " + channel
	if err := p.host.Deliver(ctx, ev, "oncall", per.contacts); err != nil {
		p.log.Warn("page not sent", "incident", inc.ID, "user", userID, "err", err)
		detail = "Not sent: " + err.Error()
	}
	return p.record(ctx, inc, event{At: now, Kind: "notified", Subject: userID, Detail: detail, Channel: channel})
}

func notifySeverity(sev string) models.Severity {
	switch sev {
	case "sev1":
		return models.SeverityCritical
	case "sev2":
		return models.SeverityError
	case "sev3":
		return models.SeverityWarning
	}
	return models.SeverityInfo
}

// incidentSeverity maps an event to sev1–sev4: the oncall_severity label wins.
func incidentSeverity(ev models.Event) string {
	switch v := ev.Labels["oncall_severity"]; v {
	case "sev1", "sev2", "sev3", "sev4":
		return v
	}
	switch ev.Severity {
	case models.SeverityCritical:
		return "sev1"
	case models.SeverityError:
		return "sev2"
	case models.SeverityWarning:
		return "sev3"
	}
	return "sev4"
}

func (p *Plugin) resolve(ctx context.Context, inc *incident, now time.Time, actor, src, note string) error {
	inc.Status = "resolved"
	inc.ResolvedAt, inc.ResolvedBy, inc.EscalatesAt = &now, actor, nil
	if err := p.record(ctx, inc, event{At: now, Kind: "resolved", Actor: actor, Source: src, Detail: note}); err != nil {
		return err
	}
	if err := p.db.dropAlerts(ctx, inc.ID); err != nil {
		return err
	}
	return p.touch(ctx, inc)
}

// ingest turns an event Notify routed to an oncall provider into an incident:
// a new firing alert opens one, a repeat adds to it, a resolve closes it.
func (p *Plugin) ingest(ctx context.Context, serviceID string, msg models.NotificationMessage) error {
	if msg.Test {
		return nil
	}
	ev := msg.Event
	if ev.Source == pageSource {
		p.log.Warn("ignored an on-call page routed back to the plugin; check the routes", "route", msg.Route)
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if msg.DeliveryID != 0 {
		if seen, err := p.db.hasDelivery(ctx, msg.DeliveryID); err != nil || seen {
			return err
		}
	}
	svc, ok := p.dir.services[serviceID]
	if !ok {
		return fmt.Errorf("oncall: unknown service %q", serviceID)
	}
	if err := p.ingestLocked(ctx, svc, ev, now); err != nil {
		return err
	}
	if msg.DeliveryID != 0 {
		return p.db.markDelivery(ctx, msg.DeliveryID, now)
	}
	return nil
}

func (p *Plugin) ingestLocked(ctx context.Context, svc *service, ev models.Event, now time.Time) error {
	fp := ev.Fingerprint
	existing, err := p.db.alert(ctx, svc.ID, fp)
	if err != nil {
		return err
	}
	inc := p.incidents[existing]
	if ev.Status == models.StatusResolved {
		// Notify delivers in parallel, so a resolve can overtake its firing:
		// remember when the alert ended, and a firing from before then is stale.
		if err := p.db.putResolved(ctx, svc.ID, fp, ev.Timestamp); err != nil {
			return err
		}
		if inc == nil || !inc.open() {
			return p.db.dropAlerts(ctx, existing)
		}
		if !svc.autoResolve {
			if err := p.record(ctx, inc, event{At: now, Kind: "alert", Source: ev.Source, Detail: "The alert resolved at the source"}); err != nil {
				return err
			}
			return p.touch(ctx, inc)
		}
		return p.resolve(ctx, inc, now, "", ev.Source, firstSentence(ev.Summary, ev.Title))
	}
	if ev.Status == models.StatusFiring {
		ended, ok, err := p.db.resolvedAt(ctx, svc.ID, fp)
		if err != nil {
			return err
		}
		if ok && !ev.Timestamp.After(ended) {
			p.log.Info("ignored a firing older than its resolve", "service", svc.ID, "fingerprint", fp)
			return nil
		}
	}
	if ev.Status == models.StatusFiring && inc != nil && inc.open() {
		inc.AlertCount++
		if err := p.record(ctx, inc, event{At: now, Kind: "alert", Source: ev.Source, Detail: firstSentence(ev.Summary, ev.Title)}); err != nil {
			return err
		}
		return p.touch(ctx, inc)
	}
	desc := ev.Description
	if desc == "" {
		desc = ev.Summary
	}
	ref := ev.Labels["alertname"]
	if ref == "" {
		ref = ev.Type
	}
	inc, err = p.openIncident(ctx, now, svc, ev.Title, desc, incidentSeverity(ev), source{Kind: ev.Source, Ref: ref, Fingerprint: fp}, "")
	if err != nil {
		return err
	}
	if ev.Status == models.StatusFiring {
		return p.db.putAlert(ctx, svc.ID, fp, inc.ID)
	}
	return nil
}

// tick escalates what is due and notes handoffs.
func (p *Plugin) tick(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, inc := range p.incidents {
		if inc.Status != "triggered" || inc.EscalatesAt == nil || now.Before(*inc.EscalatesAt) {
			continue
		}
		if err := p.escalateDue(ctx, inc, now); err != nil {
			p.log.Error("escalate", "incident", inc.ID, "err", err)
		}
	}
	if err := p.handoffs(ctx, now); err != nil {
		p.log.Error("handoffs", "err", err)
	}
}

func (p *Plugin) escalateDue(ctx context.Context, inc *incident, now time.Time) error {
	lvl, ok, wraps := p.next(inc)
	if !ok {
		inc.EscalatesAt = nil
		return p.touch(ctx, inc)
	}
	if wraps {
		inc.LoopsLeft--
	}
	if err := p.assign(ctx, inc, lvl, now, "escalated", ""); err != nil {
		return err
	}
	return p.touch(ctx, inc)
}

// handoffs records who came on call since the last look.
func (p *Plugin) handoffs(ctx context.Context, now time.Time) error {
	changed := false
	for _, sid := range p.dir.scheduleIDs {
		sc := p.dir.schedules[sid]
		cur := onCallAt(sc, p.overrides[sid], now)
		prev, known := p.lastOnCall[sid]
		if known && prev == cur.user {
			continue
		}
		p.lastOnCall[sid] = cur.user
		changed = true
		if !known || cur.user == "" {
			continue
		}
		_, lvl := p.dir.placement(sid)
		detail := fmt.Sprintf("Handoff from %s · %s until %s", p.dir.user(prev).Name, roleFor(sc, lvl),
			cur.end.In(sc.loc).Format("Mon 3:04 PM"))
		if err := p.db.addEvent(ctx, &event{At: now, Kind: "handoff", ScheduleID: sid, Subject: cur.user, Detail: detail}); err != nil {
			return err
		}
	}
	if !changed {
		return nil
	}
	p.changeSeq++
	p.oncallSeq = p.changeSeq
	if err := p.db.setMeta(ctx, "last_oncall", p.lastOnCall); err != nil {
		return err
	}
	return p.saveCounters(ctx)
}
