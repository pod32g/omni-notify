package oncall

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Push to the Omni On-Call app: each phone registers its APNs token here, and
// a page to its person also goes, as an Omni-Notify delivery, through the
// provider of kind apns that push.provider names. Notify does the sending;
// the plugin only knows whose phones are whose.

// device is one registered phone. The token is not shown back.
type device struct {
	ID          string    `json:"id"`
	Platform    string    `json:"platform"`
	Name        string    `json:"name"`
	Environment string    `json:"environment"`
	UpdatedAt   time.Time `json:"updated_at"`
	token       string
	userID      string
}

var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// ---- store ----

// putDevice saves a phone for a person. A token moves with its phone: another
// row holding the same token (a reinstall, a new owner) is dropped.
func (s *store) putDevice(ctx context.Context, d device) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE token = ? AND id <> ?`, d.token, d.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO devices (id, user_id, token, environment, name, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET user_id = excluded.user_id, token = excluded.token,
			environment = excluded.environment, name = excluded.name, updated_at = excluded.updated_at`,
		d.ID, d.userID, d.token, d.Environment, d.Name, ts(d.UpdatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) deleteDevice(ctx context.Context, userID, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *store) devices(ctx context.Context, userID string) ([]device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, token, environment, name, updated_at FROM devices
		WHERE user_id = ? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []device
	for rows.Next() {
		d := device{Platform: "ios", userID: userID}
		var at string
		if err := rows.Scan(&d.ID, &d.token, &d.Environment, &d.Name, &at); err != nil {
			return nil, err
		}
		d.UpdatedAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- API ----

func (p *Plugin) listDevices(r *http.Request, me *person) (int, any) {
	ds, err := p.db.devices(r.Context(), me.ID)
	if err != nil {
		return internal(err)
	}
	return 200, map[string]any{"devices": orEmpty(ds), "push": p.cfg.Push.Provider != ""}
}

// putDevice registers this phone, or refreshes its token:
// PUT /devices/{id} {"platform":"ios","token":"<hex>","environment":"development|production","name":"…"}.
func (p *Plugin) putDevice(r *http.Request, me *person) (int, any) {
	id := r.PathValue("id")
	if !deviceIDPattern.MatchString(id) {
		return fail(http.StatusBadRequest, "invalid", "a device id is 1–64 letters, digits or dashes")
	}
	var body struct {
		Platform    string `json:"platform"`
		Token       string `json:"token"`
		Environment string `json:"environment"`
		Name        string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		return fail(http.StatusBadRequest, "invalid", "bad JSON: "+err.Error())
	}
	tok := strings.ToLower(strings.TrimSpace(body.Token))
	switch {
	case body.Platform != "ios":
		return fail(http.StatusBadRequest, "invalid", `platform must be "ios"`)
	case len(tok) < 32 || len(tok) > 200 || !isHex(tok):
		return fail(http.StatusBadRequest, "invalid", "token must be the APNs device token in hex")
	case body.Environment != "development" && body.Environment != "production":
		return fail(http.StatusBadRequest, "invalid", `environment must be "development" or "production"`)
	}
	d := device{ID: id, Platform: "ios", Name: truncateRunes(strings.TrimSpace(body.Name), 80), Environment: body.Environment,
		UpdatedAt: p.now(), token: tok, userID: me.ID}
	if err := p.db.putDevice(r.Context(), d); err != nil {
		return internal(err)
	}
	return 200, map[string]any{"device": d, "push": p.cfg.Push.Provider != ""}
}

func (p *Plugin) deleteDevice(r *http.Request, me *person) (int, any) {
	ok, err := p.db.deleteDevice(r.Context(), me.ID, r.PathValue("id"))
	if err != nil {
		return internal(err)
	}
	if !ok {
		return fail(http.StatusNotFound, "not_found", "no such device of yours")
	}
	return 200, map[string]any{"deleted": true}
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---- paging ----

// pushAnnotations says, for the apns provider, which phones to ring and what
// the notification can do (the app's INCIDENT_TRIGGERED category). The
// payload carries the incident id and version, never its state as fact.
func pushAnnotations(inc *incident, svc Ref, ds []device, now time.Time) map[string]string {
	targets := make([]string, len(ds))
	for i, d := range ds {
		targets[i] = d.Environment + ":" + d.token
	}
	data, _ := json.Marshal(map[string]any{
		"incident_id": inc.ID, "incident_version": inc.Version + 1, "status": inc.Status,
		"severity": inc.Severity, "service": svc.Name, "sent_at": now.UTC().Format(time.RFC3339),
	})
	return map[string]string{
		"apns_tokens": strings.Join(targets, ","), "apns_category": "INCIDENT_TRIGGERED", "apns_thread_id": inc.ID,
		"apns_collapse_id": inc.ID, "apns_interruption_level": "time-sensitive", "apns_data": string(data),
	}
}

func devicesLabel(n int) string {
	if n == 1 {
		return "the Omni On-Call app"
	}
	return fmt.Sprintf("the Omni On-Call app (%d phones)", n)
}
