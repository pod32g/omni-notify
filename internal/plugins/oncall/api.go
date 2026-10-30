package oncall

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Handler serves the app's API (docs/oncall/API.md in the iOS repo) under
// /oncall/api/v1. Every request is a person: a bearer token whose SHA-256 is
// a user's token_sha256.
func (p *Plugin) Handler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn func(*http.Request, *person) (int, any)) {
		mux.HandleFunc(pattern, p.serve(fn))
	}
	h("GET /api/v1/me", p.getMe)
	h("GET /api/v1/users", p.listUsers)
	h("GET /api/v1/incidents", p.listIncidents)
	h("POST /api/v1/incidents", p.createIncident)
	h("GET /api/v1/incidents/{id}", p.getIncident)
	h("GET /api/v1/incidents/{id}/timeline", p.getTimeline)
	h("POST /api/v1/incidents/{id}/{action}", p.act)
	h("GET /api/v1/oncalls", p.listOnCalls)
	h("GET /api/v1/schedules", p.listSchedules)
	h("GET /api/v1/schedules/{id}", p.getSchedule)
	h("POST /api/v1/schedules/{id}/overrides", p.createOverride)
	h("GET /api/v1/escalation_policies", p.listPolicies)
	h("GET /api/v1/escalation_policies/{id}", p.getPolicy)
	h("GET /api/v1/services", p.listServices)
	h("GET /api/v1/services/{id}", p.getService)
	h("GET /api/v1/activity", p.listActivity)
	h("GET /api/v1/changes", p.getChanges)
	return mux
}

// serve authenticates, replays a repeated Idempotency-Key, runs fn under the
// plugin's lock and stamps the answer with as_of.
func (p *Plugin) serve(fn func(*http.Request, *person) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me := p.authenticate(r)
		if me == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="omni-oncall"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing or invalid bearer token", "code": "unauthorized"})
			return
		}
		key := r.Header.Get("Idempotency-Key")
		idempotent := r.Method == http.MethodPost && key != ""
		p.mu.Lock()
		defer p.mu.Unlock()
		if idempotent {
			status, body, ok, err := p.db.replay(r.Context(), me.ID, key)
			if err != nil {
				p.log.Error("idempotency lookup", "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error", "code": "internal"})
				return
			}
			if ok {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replay", "true")
				w.WriteHeader(status)
				_, _ = w.Write(body)
				return
			}
		}
		status, body := fn(r, me)
		if m, ok := body.(map[string]any); ok {
			m["as_of"] = p.now()
		}
		buf, err := json.Marshal(body)
		if err != nil {
			status, buf = http.StatusInternalServerError, []byte(`{"error":"internal error","code":"internal"}`)
		}
		if idempotent && status < 300 {
			if err := p.db.remember(r.Context(), me.ID, key, status, buf, p.now()); err != nil {
				p.log.Warn("idempotency store", "err", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(buf)
	}
}

func (p *Plugin) authenticate(r *http.Request) *person {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return nil
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(tok))
	var found *person
	for _, id := range p.dir.userIDs {
		if u := p.dir.users[id]; u.token != nil && subtle.ConstantTimeCompare(u.token, sum[:]) == 1 {
			found = u
		}
	}
	return found
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(status int, code, msg string) (int, any) {
	return status, map[string]any{"error": msg, "code": code}
}

func internal(err error) (int, any) {
	return fail(http.StatusInternalServerError, "internal", "internal error: "+err.Error())
}

func decode(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(v)
}

func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// ---- me and people ----

func (p *Plugin) getMe(_ *http.Request, me *person) (int, any) {
	teams := []Ref{}
	for _, id := range me.teams {
		teams = append(teams, p.dir.teamRef(id))
	}
	caps := map[string]bool{}
	for _, c := range allCapabilities {
		caps[c] = me.caps[c]
	}
	return 200, map[string]any{"user": me.User, "teams": teams, "capabilities": caps,
		"server": map[string]string{"kind": "omni-notify", "name": "Omni-Notify on-call plugin"}}
}

func (p *Plugin) listUsers(r *http.Request, _ *person) (int, any) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	out := []User{}
	for _, id := range p.dir.userIDs {
		if u := p.dir.users[id].User; q == "" || strings.Contains(strings.ToLower(u.Name), q) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return 200, map[string]any{"users": out}
}

// ---- incidents ----

func (p *Plugin) inScope(inc *incident, me *person, scope string) bool {
	switch scope {
	case "all":
		return true
	case "teams":
		return contains(me.teams, inc.TeamID)
	default:
		return contains(inc.Assignees, me.ID)
	}
}

func (p *Plugin) listIncidents(r *http.Request, me *person) (int, any) {
	q := r.URL.Query()
	statuses := map[string]bool{"triggered": true, "acknowledged": true}
	if v := q.Get("status"); v != "" {
		statuses = map[string]bool{}
		for _, st := range strings.Split(v, ",") {
			statuses[st] = true
		}
	}
	scope := q.Get("scope")
	if scope == "" {
		scope = "mine"
	}
	sevs := map[string]bool{}
	for _, v := range strings.Split(q.Get("severity"), ",") {
		if v != "" {
			sevs[v] = true
		}
	}
	since, _ := time.Parse(time.RFC3339, q.Get("since"))
	until, _ := time.Parse(time.RFC3339, q.Get("until"))
	text := strings.ToLower(q.Get("q"))
	counts := map[string]int{"mine": 0, "teams": 0, "all": 0}
	var out []*incident
	for _, inc := range p.incidents {
		if !statuses[inc.Status] || len(sevs) > 0 && !sevs[inc.Severity] {
			continue
		}
		if v := q.Get("service_id"); v != "" && inc.ServiceID != v {
			continue
		}
		if v := q.Get("team_id"); v != "" && inc.TeamID != v {
			continue
		}
		// Resolved incidents are dated by when they were resolved.
		at := inc.CreatedAt
		if inc.ResolvedAt != nil {
			at = *inc.ResolvedAt
		}
		if !since.IsZero() && at.Before(since) || !until.IsZero() && at.After(until) {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(inc.Title+" "+p.serviceRef(inc.ServiceID).Name+" #"+strconv.Itoa(inc.Number)), text) {
			continue
		}
		for _, sc := range []string{"mine", "teams", "all"} {
			if p.inScope(inc, me, sc) {
				counts[sc]++
			}
		}
		if p.inScope(inc, me, scope) {
			out = append(out, inc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	now := p.now()
	rendered := make([]Incident, 0, len(out))
	for _, inc := range out {
		rendered = append(rendered, p.render(inc, now))
	}
	return 200, map[string]any{"incidents": rendered, "counts": counts, "next_cursor": nil}
}

func (p *Plugin) getIncident(r *http.Request, _ *person) (int, any) {
	inc, ok := p.incidents[r.PathValue("id")]
	if !ok {
		return fail(404, "not_found", "no such incident")
	}
	return 200, map[string]any{"incident": p.render(inc, p.now())}
}

func (p *Plugin) getTimeline(r *http.Request, me *person) (int, any) {
	inc, ok := p.incidents[r.PathValue("id")]
	if !ok {
		return fail(404, "not_found", "no such incident")
	}
	events, err := p.db.timeline(r.Context(), inc.ID)
	if err != nil {
		return internal(err)
	}
	out := make([]Entry, 0, len(events))
	for _, e := range events {
		out = append(out, p.entry(e, me.ID))
	}
	return 200, map[string]any{"entries": out}
}

type actionBody struct {
	UserID             string `json:"user_id"`
	EscalationPolicyID string `json:"escalation_policy_id"`
	Level              int    `json:"level"`
	Body               string `json:"body"`
	Note               string `json:"note"`
}

func (p *Plugin) act(r *http.Request, me *person) (int, any) {
	inc, ok := p.incidents[r.PathValue("id")]
	if !ok {
		return fail(404, "not_found", "no such incident")
	}
	var body actionBody
	if err := decode(r, &body); err != nil {
		return fail(400, "invalid", "invalid JSON")
	}
	ctx, now := r.Context(), p.now()
	conflict := func(msg string) (int, any) {
		return 409, map[string]any{"error": msg, "code": "conflict", "incident": p.render(inc, now)}
	}
	forbidden := func() (int, any) { return fail(403, "forbidden", "you can't do that to this incident") }
	unchanged := func() (int, any) { return 200, map[string]any{"incident": p.render(inc, now)} }
	var err error
	switch r.PathValue("action") {
	case "acknowledge":
		if !contains(inc.Assignees, me.ID) && !contains(me.teams, inc.TeamID) {
			return forbidden()
		}
		switch {
		case inc.Status == "resolved":
			return conflict("the incident is already resolved")
		case inc.Status == "acknowledged" && contains(inc.AckedBy, me.ID):
			return unchanged()
		}
		inc.Status = "acknowledged"
		if inc.AckedAt == nil {
			inc.AckedAt = &now
		}
		inc.AckedBy = append(inc.AckedBy, me.ID)
		inc.EscalatesAt = nil
		err = p.record(ctx, inc, event{At: now, Kind: "acknowledged", Actor: me.ID})
	case "resolve":
		if !me.caps["resolve"] {
			return forbidden()
		}
		if inc.Status == "resolved" {
			return unchanged()
		}
		if err := p.resolve(ctx, inc, now, me.ID, "", strings.TrimSpace(body.Note)); err != nil {
			return internal(err)
		}
		return unchanged()
	case "reassign":
		if !me.caps["reassign"] {
			return forbidden()
		}
		if inc.Status == "resolved" {
			return conflict("a resolved incident can't be reassigned")
		}
		if code, msg := p.reassign(r, inc, me, body, now); code != 0 {
			return fail(code, "invalid", msg)
		}
	case "escalate":
		if !me.caps["escalate"] {
			return forbidden()
		}
		if inc.Status == "resolved" {
			return conflict("a resolved incident can't be escalated")
		}
		pol := p.dir.policies[inc.PolicyID]
		if body.Level <= inc.Level || pol == nil || body.Level > len(pol.levels) {
			return fail(400, "invalid", "level must be above the current level and within the escalation policy")
		}
		reopen(inc)
		err = p.assign(ctx, inc, body.Level, now, "escalated", me.ID)
	case "notes":
		text := strings.TrimSpace(body.Body)
		if text == "" || len([]rune(text)) > 2000 {
			return fail(400, "invalid", "a note needs 1–2000 characters")
		}
		inc.NoteCount++
		err = p.record(ctx, inc, event{At: now, Kind: "note", Actor: me.ID, Detail: text})
	default:
		return fail(404, "not_found", "no such action")
	}
	if err == nil {
		err = p.touch(ctx, inc)
	}
	if err != nil {
		return internal(err)
	}
	return unchanged()
}

// reopen puts an acknowledged incident back to triggered: someone new has it.
func reopen(inc *incident) {
	if inc.Status == "acknowledged" {
		inc.Status, inc.AckedAt, inc.AckedBy = "triggered", nil, []string{}
	}
}

// reassign hands the incident to a person (escalation carries on from the
// current level) or to another policy (from its level 1). It returns an HTTP
// status and message for bad input, or 0.
func (p *Plugin) reassign(r *http.Request, inc *incident, me *person, body actionBody, now time.Time) (int, string) {
	ctx := r.Context()
	switch {
	case body.UserID != "":
		if _, ok := p.dir.users[body.UserID]; !ok {
			return 400, "unknown user"
		}
		reopen(inc)
		inc.Assignees = []string{body.UserID}
		inc.EscalatesAt = nil
		if pol := p.dir.policies[inc.PolicyID]; pol != nil && inc.Level >= 1 && inc.Level <= len(pol.levels) {
			if _, ok, _ := p.next(inc); ok {
				t := now.Add(pol.levels[inc.Level-1].delay)
				inc.EscalatesAt = &t
			}
		}
		if err := p.record(ctx, inc, event{At: now, Kind: "reassigned", Actor: me.ID, Subject: body.UserID}); err != nil {
			return 500, err.Error()
		}
		if err := p.page(ctx, inc, body.UserID, now); err != nil {
			return 500, err.Error()
		}
	case body.EscalationPolicyID != "":
		pol, ok := p.dir.policies[body.EscalationPolicyID]
		if !ok {
			return 400, "unknown escalation policy"
		}
		reopen(inc)
		inc.PolicyID, inc.LoopsLeft = pol.ID, pol.repeat
		if err := p.assign(ctx, inc, 1, now, "reassigned", me.ID); err != nil {
			return 500, err.Error()
		}
	default:
		return 400, "user_id or escalation_policy_id is required"
	}
	return 0, ""
}

func (p *Plugin) createIncident(r *http.Request, me *person) (int, any) {
	if !me.caps["create_incident"] {
		return fail(403, "forbidden", "you can't open incidents")
	}
	var body struct {
		Title       string     `json:"title"`
		Description string     `json:"description"`
		Severity    string     `json:"severity"`
		ServiceID   string     `json:"service_id"`
		AssignTo    actionBody `json:"assign_to"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Title) == "" {
		return fail(400, "invalid", "title is required")
	}
	svc, ok := p.dir.services[body.ServiceID]
	if !ok {
		return fail(400, "invalid", "unknown service")
	}
	switch body.Severity {
	case "sev1", "sev2", "sev3", "sev4":
	default:
		return fail(400, "invalid", "severity must be sev1–sev4")
	}
	now := p.now()
	inc, err := p.openIncident(r.Context(), now, svc, strings.TrimSpace(body.Title), strings.TrimSpace(body.Description),
		body.Severity, source{Kind: "manual", Ref: me.Name}, me.ID)
	if err != nil {
		return internal(err)
	}
	if body.AssignTo.UserID != "" || body.AssignTo.EscalationPolicyID != "" {
		if code, msg := p.reassign(r, inc, me, body.AssignTo, now); code != 0 {
			return fail(code, "invalid", msg)
		}
		if err := p.touch(r.Context(), inc); err != nil {
			return internal(err)
		}
	}
	return 201, map[string]any{"incident": p.render(inc, now)}
}

// ---- on call ----

func (p *Plugin) listOnCalls(r *http.Request, me *person) (int, any) {
	now := p.now()
	out := []Shift{}
	if r.URL.Query().Get("scope") == "now" {
		// Who each policy level pages through a schedule right now. Users a
		// level names directly have no shift and show on the policy instead.
		for _, pid := range p.dir.policyIDs {
			pol := p.dir.policies[pid]
			for i, l := range pol.levels {
				for _, sid := range l.schedules {
					sc := p.dir.schedules[sid]
					if s := onCallAt(sc, p.overrides[sid], now); s.user != "" {
						out = append(out, p.dir.shift(sc, s, pol.Ref, i+1))
					}
				}
			}
		}
		return 200, map[string]any{"shifts": out}
	}
	for _, sid := range p.dir.scheduleIDs {
		sc := p.dir.schedules[sid]
		pol, lvl := p.dir.placement(sid)
		for _, s := range spans(sc, p.overrides[sid], now, now.AddDate(0, 0, 14)) {
			if s.user == me.ID && s.end.After(now) {
				out = append(out, p.dir.shift(sc, s, pol, lvl))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return 200, map[string]any{"shifts": out}
}

func (p *Plugin) scheduleSummary(sc *schedule, now time.Time) map[string]any {
	pol, lvl := p.dir.placement(sc.ID)
	return map[string]any{"id": sc.ID, "name": sc.Name, "timezone": sc.tz, "team": sc.team,
		"current": p.dir.shift(sc, onCallAt(sc, p.overrides[sc.ID], now), pol, lvl)}
}

func (p *Plugin) listSchedules(_ *http.Request, _ *person) (int, any) {
	now := p.now()
	out := []map[string]any{}
	for _, sid := range p.dir.scheduleIDs {
		out = append(out, p.scheduleSummary(p.dir.schedules[sid], now))
	}
	return 200, map[string]any{"schedules": out}
}

func (p *Plugin) getSchedule(r *http.Request, _ *person) (int, any) {
	sc, ok := p.dir.schedules[r.PathValue("id")]
	if !ok {
		return fail(404, "not_found", "no such schedule")
	}
	now := p.now()
	since, until := now.Add(-24*time.Hour), now.AddDate(0, 0, 14)
	if t, err := time.Parse(time.RFC3339, r.URL.Query().Get("since")); err == nil {
		since = t
	}
	if t, err := time.Parse(time.RFC3339, r.URL.Query().Get("until")); err == nil {
		until = t
	}
	if !until.After(since) || until.Sub(since) > 62*24*time.Hour {
		return fail(400, "invalid", "until must be after since, at most 62 days later")
	}
	pol, lvl := p.dir.placement(sc.ID)
	shifts := []Shift{}
	for _, s := range spans(sc, p.overrides[sc.ID], since, until) {
		shifts = append(shifts, p.dir.shift(sc, s, pol, lvl))
	}
	ovs := []Shift{}
	for _, o := range p.overrides[sc.ID] {
		if o.End.After(since) {
			ovs = append(ovs, p.dir.shift(sc, span{user: o.UserID, start: o.Start, end: o.End, override: true}, pol, lvl))
		}
	}
	layer := map[string]any{"name": capitalize(sc.rotation) + " rotation", "rotation": sc.rotation, "handoff": sc.handoff,
		"users": p.dir.usersOf(sc.users)}
	return 200, map[string]any{"schedule": p.scheduleSummary(sc, now), "spans": shifts,
		"layers": []map[string]any{layer}, "overrides": ovs}
}

func (p *Plugin) createOverride(r *http.Request, me *person) (int, any) {
	if !me.caps["manage_overrides"] {
		return fail(403, "forbidden", "you can't change schedules")
	}
	sc, ok := p.dir.schedules[r.PathValue("id")]
	if !ok {
		return fail(404, "not_found", "no such schedule")
	}
	var body struct {
		UserID string    `json:"user_id"`
		Start  time.Time `json:"start"`
		End    time.Time `json:"end"`
	}
	if err := decode(r, &body); err != nil || !body.End.After(body.Start) {
		return fail(400, "invalid", "start and end are required, end after start")
	}
	if _, ok := p.dir.users[body.UserID]; !ok {
		return fail(400, "invalid", "unknown user")
	}
	ctx, now := r.Context(), p.now()
	o := override{ID: fmt.Sprintf("ov_%d", p.changeSeq+1), ScheduleID: sc.ID, UserID: body.UserID,
		Start: body.Start.UTC(), End: body.End.UTC(), CreatedBy: me.ID, CreatedAt: now}
	if err := p.db.putOverride(ctx, o); err != nil {
		return internal(err)
	}
	p.overrides[sc.ID] = append(p.overrides[sc.ID], o)
	detail := o.Start.In(sc.loc).Format("Mon 3:04 PM") + " – " + o.End.In(sc.loc).Format("Mon 3:04 PM")
	if err := p.db.addEvent(ctx, &event{At: now, Kind: "override", ScheduleID: sc.ID, Actor: me.ID, Subject: o.UserID, Detail: detail}); err != nil {
		return internal(err)
	}
	p.changeSeq++
	p.oncallSeq = p.changeSeq
	if err := p.saveCounters(ctx); err != nil {
		return internal(err)
	}
	pol, lvl := p.dir.placement(sc.ID)
	return 201, map[string]any{"override": p.dir.shift(sc, span{user: o.UserID, start: o.Start, end: o.End, override: true}, pol, lvl)}
}

// ---- policies and services ----

type levelView struct {
	Level        int          `json:"level"`
	DelayMinutes int          `json:"delay_minutes"`
	Targets      []targetView `json:"targets"`
}

type targetView struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	OnCall []User `json:"oncall"`
}

func (p *Plugin) policyView(pol *policy, now time.Time) map[string]any {
	levels := []levelView{}
	for i, l := range pol.levels {
		v := levelView{Level: i + 1, DelayMinutes: int(l.delay.Minutes()), Targets: []targetView{}}
		for _, sid := range l.schedules {
			sc := p.dir.schedules[sid]
			s := onCallAt(sc, p.overrides[sid], now)
			oncall := []User{}
			if s.user != "" {
				oncall = append(oncall, p.dir.user(s.user))
			}
			v.Targets = append(v.Targets, targetView{Kind: "schedule", ID: sid, Name: sc.Name, OnCall: oncall})
		}
		for _, uid := range l.users {
			u := p.dir.user(uid)
			v.Targets = append(v.Targets, targetView{Kind: "user", ID: uid, Name: u.Name, OnCall: []User{u}})
		}
		levels = append(levels, v)
	}
	return map[string]any{"id": pol.ID, "name": pol.Name, "team": pol.team, "repeat": pol.repeat, "levels": levels}
}

func (p *Plugin) listPolicies(_ *http.Request, _ *person) (int, any) {
	now := p.now()
	out := []map[string]any{}
	for _, id := range p.dir.policyIDs {
		out = append(out, p.policyView(p.dir.policies[id], now))
	}
	return 200, map[string]any{"escalation_policies": out}
}

func (p *Plugin) getPolicy(r *http.Request, _ *person) (int, any) {
	pol, ok := p.dir.policies[r.PathValue("id")]
	if !ok {
		return fail(404, "not_found", "no such escalation policy")
	}
	return 200, map[string]any{"escalation_policy": p.policyView(pol, p.now())}
}

type serviceView struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Description      string     `json:"description"`
	Status           string     `json:"status"`
	Team             Ref        `json:"team"`
	EscalationPolicy Ref        `json:"escalation_policy"`
	OpenIncidents    int        `json:"open_incidents"`
	LastIncidentAt   *time.Time `json:"last_incident_at"`
	Health           []struct{} `json:"health"`
}

// serviceView derives a service's status from its open incidents: critical
// with a SEV1 open, degraded with a SEV2 or SEV3.
func (p *Plugin) serviceView(svc *service) serviceView {
	v := serviceView{ID: svc.ID, Name: svc.Name, Description: svc.description, Status: "operational", Team: svc.team,
		EscalationPolicy: svc.policy, Health: []struct{}{}}
	worst := ""
	for _, inc := range p.incidents {
		if inc.ServiceID != svc.ID {
			continue
		}
		if v.LastIncidentAt == nil || inc.CreatedAt.After(*v.LastIncidentAt) {
			t := inc.CreatedAt
			v.LastIncidentAt = &t
		}
		if inc.open() {
			v.OpenIncidents++
			if worst == "" || inc.Severity < worst {
				worst = inc.Severity
			}
		}
	}
	switch worst {
	case "sev1":
		v.Status = "critical"
	case "sev2", "sev3":
		v.Status = "degraded"
	}
	return v
}

func (p *Plugin) listServices(_ *http.Request, _ *person) (int, any) {
	out := []serviceView{}
	for _, id := range p.dir.serviceIDs {
		out = append(out, p.serviceView(p.dir.services[id]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return 200, map[string]any{"services": out}
}

func (p *Plugin) getService(r *http.Request, _ *person) (int, any) {
	svc, ok := p.dir.services[r.PathValue("id")]
	if !ok {
		return fail(404, "not_found", "no such service")
	}
	return 200, map[string]any{"service": p.serviceView(svc)}
}

// ---- activity and sync ----

type activityView struct {
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	Summary    string    `json:"summary"`
	Detail     string    `json:"detail"`
	IncidentID *string   `json:"incident_id"`
	ServiceID  *string   `json:"service_id"`
	ScheduleID *string   `json:"schedule_id"`
	Actor      *User     `json:"actor"`
}

var activityKinds = map[string][]string{
	"incident":   {"triggered", "acknowledged", "resolved", "note", "reassigned"},
	"escalation": {"escalated"},
	"oncall":     {"handoff", "override"},
}

func (p *Plugin) listActivity(r *http.Request, me *person) (int, any) {
	var kinds []string
	filters := strings.Split(r.URL.Query().Get("kinds"), ",")
	for _, f := range []string{"incident", "escalation", "oncall"} {
		if contains(filters, f) || r.URL.Query().Get("kinds") == "" {
			kinds = append(kinds, activityKinds[f]...)
		}
	}
	out := []activityView{}
	if len(kinds) > 0 {
		events, err := p.db.activity(r.Context(), kinds, 100)
		if err != nil {
			return internal(err)
		}
		for _, e := range events {
			out = append(out, p.activity(e, me.ID))
		}
	}
	return 200, map[string]any{"events": out, "next_cursor": nil}
}

func (p *Plugin) activity(e event, viewer string) activityView {
	en := p.entry(e, viewer)
	v := activityView{ID: "act_" + itoa(e.Seq), At: e.At, Kind: e.Kind, Summary: en.Summary, Detail: e.Detail, Actor: en.Actor}
	if e.ScheduleID != "" {
		sid := e.ScheduleID
		v.ScheduleID = &sid
	}
	if inc, ok := p.incidents[e.IncidentID]; ok {
		id, svc := inc.ID, inc.ServiceID
		v.IncidentID, v.ServiceID = &id, &svc
		v.Detail = fmt.Sprintf("#%d · %s", inc.Number, inc.Title)
		switch e.Kind {
		case "triggered":
			v.Summary = fmt.Sprintf("%s triggered on %s", strings.ToUpper(inc.Severity), p.serviceRef(inc.ServiceID).Name)
		case "acknowledged", "resolved":
			v.Summary = fmt.Sprintf("%s #%d", en.Summary, inc.Number)
		}
	}
	return v
}

func (p *Plugin) getChanges(r *http.Request, _ *person) (int, any) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	ids := []string{}
	for _, inc := range p.incidents {
		if inc.Changed > since {
			ids = append(ids, inc.ID)
		}
	}
	sort.Strings(ids)
	return 200, map[string]any{"incident_ids": ids, "oncall_changed": since == 0 || p.oncallSeq > since,
		"cursor": itoa(p.changeSeq)}
}
