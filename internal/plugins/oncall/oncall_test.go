package oncall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-notify/internal/clock"
	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/plugin"
	"github.com/pod32g/omni-notify/internal/providers"
	"gopkg.in/yaml.v3"
)

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// testConfig: Ana and Ben rotate daily on primary, Cy is secondary, Dee has
// no capabilities. Escalation: primary, then secondary, after 5 minutes.
func testConfig(dbPath string) string {
	return `
enabled: true
storage: {path: ` + dbPath + `}
users:
  - {id: ana, name: Ana Abe, timezone: America/Los_Angeles, token_sha256: ` + tokenHash("ana-token") + `, contacts: [telegram-ana]}
  - {id: ben, name: Ben Bo, timezone: America/Los_Angeles, token_sha256: ` + tokenHash("ben-token") + `, contacts: [telegram-ben, ntfy-ben]}
  - {id: cy, name: Cy Cole, token_sha256: ` + tokenHash("cy-token") + `}
  - {id: dee, name: Dee Doe, token_sha256: ` + tokenHash("dee-token") + `, capabilities: []}
teams:
  - {id: media, name: Media, members: [ana, ben, cy]}
schedules:
  - {id: primary, name: Media primary, team: media, timezone: America/Los_Angeles, rotation: daily, handoff: "09:00", start: "2026-10-26", users: [ana, ben]}
  - {id: secondary, name: Media secondary, team: media, timezone: America/Los_Angeles, rotation: weekly, handoff: "09:00", start: "2026-10-26", users: [cy]}
escalation_policies:
  - id: ep_media
    name: Media escalation
    team: media
    levels:
      - {delay: 5m, schedules: [primary]}
      - {delay: 5m, schedules: [secondary]}
services:
  - {id: jellyfin, name: jellyfin, escalation_policy: ep_media}
  - {id: backups, name: backups, escalation_policy: ep_media, auto_resolve: false}
`
}

type page struct {
	ev        models.Event
	providers []string
}

type harness struct {
	t     *testing.T
	p     *Plugin
	clk   *clock.Fake
	mu    sync.Mutex
	pages []page
	reg   *providers.Registry
	h     http.Handler
}

func newHarness(t *testing.T, dbPath string, at time.Time) *harness {
	t.Helper()
	return newHarnessFrom(t, testConfig(dbPath), at)
}

func newHarnessFrom(t *testing.T, cfg string, at time.Time) *harness {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(cfg), &node); err != nil {
		t.Fatal(err)
	}
	pl, err := New(*node.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	hs := &harness{t: t, p: pl.(*Plugin), clk: clock.NewFake(at), reg: providers.NewRegistry()}
	host := plugin.Host{Clock: hs.clk, Providers: hs.reg, Deliver: func(_ context.Context, ev models.Event, _ string, to []string) error {
		hs.mu.Lock()
		defer hs.mu.Unlock()
		hs.pages = append(hs.pages, page{ev, to})
		return nil
	}}
	if err := hs.p.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hs.p.Stop)
	hs.h = hs.p.Handler()
	return hs
}

func (hs *harness) send(service string, ev models.Event, deliveryID int64) {
	hs.t.Helper()
	prov, err := hs.reg.Build("oncall", map[string]any{"service": service}, "")
	if err != nil {
		hs.t.Fatal(err)
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = hs.clk.Now()
	}
	if err := prov.Send(context.Background(), models.NotificationMessage{Event: ev, DeliveryID: deliveryID}); err != nil {
		hs.t.Fatal(err)
	}
}

func (hs *harness) call(method, path, token, key string, body any) (int, map[string]any) {
	hs.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (hs *harness) incident(id string) *incident {
	hs.p.mu.Lock()
	defer hs.p.mu.Unlock()
	inc := *hs.p.incidents[id]
	return &inc
}

func firing(fp, title string) models.Event {
	return models.Event{EventID: fp, Type: "alert", Source: "omni-metrics", Status: models.StatusFiring,
		Severity: models.SeverityCritical, Title: title, Summary: "5xx above 20%. Streams drop.",
		Labels: map[string]string{"alertname": "JellyfinHigh5xx"}, Fingerprint: fp}
}

// Monday 2026-10-26 10:00 in Los Angeles: Ana's first daily turn.
var monday10 = time.Date(2026, 10, 26, 17, 0, 0, 0, time.UTC)

func TestRotationKeepsWallClockAcrossDaylightSaving(t *testing.T) {
	hs := newHarness(t, filepath.Join(t.TempDir(), "oc.db"), monday10)
	sc := hs.p.dir.schedules["primary"]
	la := sc.loc
	// US clocks go back on 1 November 2026: the turn from Saturday 09:00 to
	// Sunday 09:00 is 25 hours, and both ends are 09:00 local.
	sat := time.Date(2026, 10, 31, 12, 0, 0, 0, la)
	s := onCallAt(sc, nil, sat)
	if got := s.end.Sub(s.start); got != 25*time.Hour {
		t.Fatalf("turn across the change is %v, want 25h", got)
	}
	if s.start.In(la).Hour() != 9 || s.end.In(la).Hour() != 9 {
		t.Fatalf("handoffs at %v and %v, want 09:00 local", s.start.In(la), s.end.In(la))
	}
	// Day 5 after the start is Ben's (ana, ben, ana, ben, ana, ben).
	if s.user != "ben" {
		t.Fatalf("Saturday is %s's, want ben", s.user)
	}
	if onCallAt(sc, nil, time.Date(2026, 10, 26, 8, 59, 0, 0, la)).user != "ben" {
		t.Fatal("before the first handoff the previous turn (ben) should cover")
	}
}

func TestOverrideCarvesTheRotation(t *testing.T) {
	hs := newHarness(t, filepath.Join(t.TempDir(), "oc.db"), monday10)
	sc := hs.p.dir.schedules["primary"]
	start, end := monday10.Add(2*time.Hour), monday10.Add(4*time.Hour)
	ovs := []override{{ID: "o1", ScheduleID: "primary", UserID: "cy", Start: start, End: end}}
	got := spans(sc, ovs, monday10, monday10.Add(23*time.Hour))
	var users []string
	for _, s := range got {
		users = append(users, s.user)
	}
	if strings.Join(users, ",") != "ana,cy,ana" {
		t.Fatalf("spans are %v, want ana,cy,ana", users)
	}
	if !got[1].override || !got[0].end.Equal(start) || !got[2].start.Equal(end) {
		t.Fatalf("override not carved cleanly: %+v", got)
	}
}

func TestAlertsOpenAddToAndResolveIncidents(t *testing.T) {
	hs := newHarness(t, filepath.Join(t.TempDir(), "oc.db"), monday10)
	hs.send("jellyfin", firing("fp1", "Jellyfin streams failing"), 1)
	inc := hs.incident("inc_1")
	if inc.Severity != "sev1" || inc.Status != "triggered" || strings.Join(inc.Assignees, ",") != "ana" {
		t.Fatalf("opened %+v", inc)
	}
	if len(hs.pages) != 1 || hs.pages[0].providers[0] != "telegram-ana" || hs.pages[0].ev.Source != pageSource {
		t.Fatalf("pages %+v", hs.pages)
	}
	if !strings.HasPrefix(hs.pages[0].ev.Title, "SEV1 · jellyfin") || hs.pages[0].ev.Labels["oncall_incident"] != "inc_1" {
		t.Fatalf("page event %+v", hs.pages[0].ev)
	}
	// Notify retrying the same delivery changes nothing; a new firing adds to it.
	hs.send("jellyfin", firing("fp1", "Jellyfin streams failing"), 1)
	hs.send("jellyfin", firing("fp1", "Jellyfin streams failing"), 2)
	if inc := hs.incident("inc_1"); inc.AlertCount != 2 {
		t.Fatalf("alert count %d, want 2", inc.AlertCount)
	}
	resolved := firing("fp1", "Jellyfin streams failing")
	resolved.Status = models.StatusResolved
	hs.send("jellyfin", resolved, 3)
	if inc := hs.incident("inc_1"); inc.Status != "resolved" || inc.ResolvedBy != "" {
		t.Fatalf("after resolve %+v", inc)
	}
	// It fires again later: a new incident.
	hs.clk.Advance(time.Minute)
	hs.send("jellyfin", firing("fp1", "Jellyfin streams failing"), 4)
	if _, ok := hs.p.incidents["inc_2"]; !ok {
		t.Fatal("a firing after the resolve should open inc_2")
	}
	// Without auto_resolve, a resolve is only noted.
	hs.send("backups", firing("fp2", "Backup failed"), 5)
	r2 := firing("fp2", "Backup failed")
	r2.Status = models.StatusResolved
	hs.send("backups", r2, 6)
	if inc := hs.incident("inc_3"); inc.Status != "triggered" {
		t.Fatalf("backups auto-resolved: %+v", inc)
	}
	// A page routed back in is ignored.
	hs.send("jellyfin", hs.pages[0].ev, 7)
	if len(hs.p.incidents) != 3 {
		t.Fatalf("%d incidents, the routed-back page opened one", len(hs.p.incidents))
	}
}

func TestAResolveThatOvertakesItsFiringWins(t *testing.T) {
	hs := newHarness(t, filepath.Join(t.TempDir(), "oc.db"), monday10)
	fire := firing("fp1", "Down")
	fire.Timestamp = monday10.Add(-time.Minute)
	resolve := fire
	resolve.Status, resolve.Timestamp = models.StatusResolved, monday10
	hs.send("jellyfin", resolve, 1)
	hs.send("jellyfin", fire, 2)
	if len(hs.p.incidents) != 0 || len(hs.pages) != 0 {
		t.Fatalf("a stale firing opened %d incidents and paged %d times", len(hs.p.incidents), len(hs.pages))
	}
	again := fire
	again.Timestamp = monday10.Add(time.Minute)
	hs.send("jellyfin", again, 3)
	if len(hs.p.incidents) != 1 {
		t.Fatal("a firing after the resolve should open an incident")
	}
}

func TestEscalatesWhenNobodyAcknowledges(t *testing.T) {
	hs := newHarness(t, filepath.Join(t.TempDir(), "oc.db"), monday10)
	hs.send("jellyfin", firing("fp1", "Down"), 1)
	_, body := hs.call("GET", "/api/v1/incidents/inc_1", "ana-token", "", nil)
	inc := body["incident"].(map[string]any)
	if next := inc["next_responders"].([]any); len(next) != 1 || next[0].(map[string]any)["id"] != "cy" {
		t.Fatalf("next responders %v, want cy", next)
	}
	hs.clk.Advance(5*time.Minute + time.Second)
	hs.p.tick(context.Background())
	got := hs.incident("inc_1")
	if got.Level != 2 || strings.Join(got.Assignees, ",") != "cy" || got.EscalatesAt != nil {
		t.Fatalf("after the delay %+v", got)
	}
	// Cy has no contacts: the timeline says so instead of pretending.
	_, tl := hs.call("GET", "/api/v1/incidents/inc_1/timeline", "cy-token", "", nil)
	first := tl["entries"].([]any)[0].(map[string]any)
	if first["summary"] != "Notified you" || first["detail"] != "Not paged: no contact methods" {
		t.Fatalf("latest entry %v", first)
	}
}

func TestActionsFollowTheRules(t *testing.T) {
	hs := newHarness(t, filepath.Join(t.TempDir(), "oc.db"), monday10)
	hs.send("jellyfin", firing("fp1", "Down"), 1)

	if code, _ := hs.call("GET", "/api/v1/me", "", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := hs.call("GET", "/api/v1/me", "wrong", "", nil); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	code, me := hs.call("GET", "/api/v1/me", "ana-token", "", nil)
	if code != 200 || me["user"].(map[string]any)["id"] != "ana" || me["as_of"] == nil {
		t.Fatalf("me %d %v", code, me)
	}
	if caps := me["capabilities"].(map[string]any); caps["resolve"] != true || caps["request_swaps"] != false {
		t.Fatalf("capabilities %v", caps)
	}

	// Dee isn't assigned and isn't on the team.
	if code, _ := hs.call("POST", "/api/v1/incidents/inc_1/acknowledge", "dee-token", "", nil); code != 403 {
		t.Fatalf("outsider acknowledged: %d", code)
	}
	code, a := hs.call("POST", "/api/v1/incidents/inc_1/acknowledge", "ana-token", "k1", nil)
	v1 := a["incident"].(map[string]any)["version"]
	if code != 200 || a["incident"].(map[string]any)["status"] != "acknowledged" {
		t.Fatalf("acknowledge %d %v", code, a)
	}
	_, replay := hs.call("POST", "/api/v1/incidents/inc_1/acknowledge", "ana-token", "k1", nil)
	if replay["incident"].(map[string]any)["version"] != v1 {
		t.Fatal("a repeated key should replay, not act again")
	}
	_, tl := hs.call("GET", "/api/v1/incidents/inc_1/timeline", "ben-token", "", nil)
	if s := tl["entries"].([]any)[0].(map[string]any)["summary"]; s != "Ana Abe acknowledged" {
		t.Fatalf("Ben reads %q", s)
	}

	if code, _ := hs.call("POST", "/api/v1/incidents/inc_1/notes", "ben-token", "", map[string]string{"body": " "}); code != 400 {
		t.Fatalf("empty note: %d", code)
	}
	if code, _ := hs.call("POST", "/api/v1/incidents/inc_1/resolve", "dee-token", "", nil); code != 403 {
		t.Fatalf("resolve without the capability: %d", code)
	}
	if code, _ := hs.call("POST", "/api/v1/incidents/inc_1/resolve", "ben-token", "", map[string]string{"note": "restarted"}); code != 200 {
		t.Fatalf("resolve: %d", code)
	}
	code, c := hs.call("POST", "/api/v1/incidents/inc_1/acknowledge", "cy-token", "", nil)
	if code != 409 || c["incident"].(map[string]any)["status"] != "resolved" {
		t.Fatalf("acknowledging a resolved incident: %d %v", code, c)
	}

	// The change cursor moves, and on-call is fresh on a first sync.
	_, ch := hs.call("GET", "/api/v1/changes?since=0", "ana-token", "", nil)
	if ch["oncall_changed"] != true || len(ch["incident_ids"].([]any)) != 1 {
		t.Fatalf("changes %v", ch)
	}
	_, ch2 := hs.call("GET", "/api/v1/changes?since="+ch["cursor"].(string), "ana-token", "", nil)
	if len(ch2["incident_ids"].([]any)) != 0 || ch2["oncall_changed"] != false {
		t.Fatalf("nothing changed, got %v", ch2)
	}
}

func TestManualIncidentAndOverride(t *testing.T) {
	hs := newHarness(t, filepath.Join(t.TempDir(), "oc.db"), monday10)
	code, out := hs.call("POST", "/api/v1/incidents", "ben-token", "", map[string]any{
		"title": "Can't log in", "severity": "sev2", "service_id": "jellyfin",
		"assign_to": map[string]string{"user_id": "cy"}})
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	inc := out["incident"].(map[string]any)
	if inc["source"].(map[string]any)["kind"] != "manual" || inc["assignees"].([]any)[0].(map[string]any)["id"] != "cy" {
		t.Fatalf("created %v", inc)
	}
	start := monday10.Add(time.Hour)
	code, _ = hs.call("POST", "/api/v1/schedules/primary/overrides", "ben-token", "", map[string]any{
		"user_id": "ben", "start": start, "end": start.Add(2 * time.Hour)})
	if code != 201 {
		t.Fatalf("override %d", code)
	}
	hs.clk.Advance(90 * time.Minute)
	_, now := hs.call("GET", "/api/v1/oncalls?scope=now", "ana-token", "", nil)
	shifts := now["shifts"].([]any)
	first := shifts[0].(map[string]any)
	if first["user"].(map[string]any)["id"] != "ben" || first["is_override"] != true || first["role"] != "Primary" {
		t.Fatalf("on call now %v", first)
	}
	_, feed := hs.call("GET", "/api/v1/activity?kinds=oncall", "ben-token", "", nil)
	if s := feed["events"].([]any)[0].(map[string]any)["summary"]; s != "You cover Media primary" {
		t.Fatalf("feed reads %q", s)
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "oc.db")
	hs := newHarness(t, db, monday10)
	hs.send("jellyfin", firing("fp1", "Down"), 1)
	hs.p.Stop()

	again := newHarness(t, db, monday10.Add(time.Minute))
	if inc := again.incident("inc_1"); inc.Status != "triggered" || inc.Version == 0 {
		t.Fatalf("after restart %+v", inc)
	}
	// The open alert still maps to it, and numbering carries on.
	again.send("jellyfin", firing("fp1", "Down"), 2)
	again.send("jellyfin", firing("fp9", "Other"), 3)
	if again.incident("inc_1").AlertCount != 2 || again.p.incidents["inc_2"] == nil {
		t.Fatal("restart lost the alert map or the numbering")
	}
}

func TestConfigRejectsBrokenReferences(t *testing.T) {
	for name, cfg := range map[string]string{
		"unknown schedule": `users: [{id: a, name: A}]
escalation_policies: [{id: p, name: P, levels: [{schedules: [nope]}]}]`,
		"bad rotation": `users: [{id: a, name: A}]
schedules: [{id: s, name: S, rotation: hourly, start: "2026-10-26", users: [a]}]`,
		"bad token":      `users: [{id: a, name: A, token_sha256: abc}]`,
		"unknown policy": `services: [{id: s, name: S, escalation_policy: nope}]`,
	} {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(cfg), &node); err != nil {
			t.Fatal(err)
		}
		if _, err := New(*node.Content[0]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
