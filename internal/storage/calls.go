package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// ErrConflict is returned when a conditional update lost a race (the row was
// no longer in the expected state) or a uniqueness rule was hit.
var ErrConflict = errors.New("conflict")

const selectCallCols = `SELECT id, event_ref, agent, caller_name, simulated, test, recipient, type,
	severity, title, message, details, interactive, user_requested, idempotency_key, state,
	outcome, reason, attempt, parent_call_id, origin, scheduled_at, expires_at, ring_deadline,
	rang_at, answered_at, answered_by, ended_at, last_activity_at, created_at, updated_at`

// InsertCall stores a new call and its first log entry in one transaction. A
// call whose (agent, idempotency_key) already exists is not inserted: the
// existing call is returned with ErrConflict.
func (s *Store) InsertCall(ctx context.Context, c models.Call, actor, detail string) (models.Call, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return models.Call{}, fmt.Errorf("begin insert call: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if c.IdempotencyKey != "" {
		existing, err := scanCall(tx.QueryRowContext(ctx, selectCallCols+
			` FROM voice_calls WHERE agent = ? AND idempotency_key = ?`, c.Agent, c.IdempotencyKey))
		if err == nil {
			return existing, ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return models.Call{}, err
		}
	}
	if err := insertCallTx(ctx, tx, c); err != nil {
		return models.Call{}, err
	}
	if err := insertLogTx(ctx, tx, models.CallLogEntry{CallID: c.ID, At: c.CreatedAt, Kind: "created",
		To: c.State, Actor: actor, Detail: detail}); err != nil {
		return models.Call{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.Call{}, fmt.Errorf("commit insert call: %w", err)
	}
	return c, nil
}

func insertCallTx(ctx context.Context, tx *sql.Tx, c models.Call) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO voice_calls (id, event_ref, agent, caller_name, simulated, test, recipient, type,
			severity, title, message, details, interactive, user_requested, idempotency_key, state,
			outcome, reason, attempt, parent_call_id, origin, scheduled_at, expires_at, ring_deadline,
			rang_at, answered_at, answered_by, ended_at, last_activity_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.EventRef, c.Agent, c.CallerName, boolInt(c.Simulated), boolInt(c.Test), c.Recipient, c.Type,
		string(c.Severity), c.Title, c.Message, c.Details, boolInt(c.Interactive), boolInt(c.UserRequested),
		c.IdempotencyKey, string(c.State), string(c.Outcome), c.Reason, c.Attempt, c.ParentCallID, c.Origin,
		fmtTime(c.ScheduledAt), fmtTime(c.ExpiresAt), fmtOptTime(c.RingDeadline), fmtOptTime(c.RangAt),
		fmtOptTime(c.AnsweredAt), c.AnsweredBy, fmtOptTime(c.EndedAt), fmtTime(c.LastActivityAt),
		fmtTime(c.CreatedAt), fmtTime(c.UpdatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrConflict
		}
		return fmt.Errorf("insert call: %w", err)
	}
	return nil
}

// GetCall returns a call by id.
func (s *Store) GetCall(ctx context.Context, id string) (models.Call, error) {
	c, err := scanCall(s.db.QueryRowContext(ctx, selectCallCols+` FROM voice_calls WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Call{}, ErrNotFound
	}
	return c, err
}

// CallByIdempotencyKey returns an agent's call made with key.
func (s *Store) CallByIdempotencyKey(ctx context.Context, agent, key string) (models.Call, error) {
	c, err := scanCall(s.db.QueryRowContext(ctx, selectCallCols+
		` FROM voice_calls WHERE agent = ? AND idempotency_key = ?`, agent, key))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Call{}, ErrNotFound
	}
	return c, err
}

// CountTurns counts a call's turns with any of the given intents.
func (s *Store) CountTurns(ctx context.Context, callID string, intents ...string) (int, error) {
	q := `SELECT COUNT(*) FROM voice_turns WHERE call_id = ?`
	args := []any{callID}
	if len(intents) > 0 {
		q += ` AND intent IN (?` + strings.Repeat(",?", len(intents)-1) + `)`
		for _, in := range intents {
			args = append(args, in)
		}
	}
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// CallFilter narrows ListCalls. Zero-value fields are ignored.
type CallFilter struct {
	Recipient string
	Agent     string
	States    []models.CallState
	Limit     int
}

// ListCalls returns calls newest-first, or soonest-first when listing only
// scheduled calls.
func (s *Store) ListCalls(ctx context.Context, f CallFilter) ([]models.Call, error) {
	var where []string
	var args []any
	if f.Recipient != "" {
		where = append(where, "recipient = ?")
		args = append(args, f.Recipient)
	}
	if f.Agent != "" {
		where = append(where, "agent = ?")
		args = append(args, f.Agent)
	}
	if len(f.States) > 0 {
		ph := make([]string, len(f.States))
		for i, st := range f.States {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "state IN ("+strings.Join(ph, ",")+")")
	}
	q := selectCallCols + ` FROM voice_calls`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if len(f.States) == 1 && f.States[0] == models.CallScheduled {
		q += " ORDER BY scheduled_at ASC, id ASC"
	} else {
		q += " ORDER BY created_at DESC, id DESC"
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q += " LIMIT ?"
	args = append(args, limit)
	return s.queryCalls(ctx, q, args...)
}

// DueCalls returns scheduled calls whose time has come, soonest first.
func (s *Store) DueCalls(ctx context.Context, now time.Time, limit int) ([]models.Call, error) {
	return s.queryCalls(ctx, selectCallCols+` FROM voice_calls
		WHERE state = 'scheduled' AND scheduled_at <= ? ORDER BY scheduled_at ASC, id ASC LIMIT ?`,
		fmtTime(now), limit)
}

// RingingPastDeadline returns ringing calls whose ring deadline has passed.
func (s *Store) RingingPastDeadline(ctx context.Context, now time.Time) ([]models.Call, error) {
	return s.queryCalls(ctx, selectCallCols+` FROM voice_calls
		WHERE state = 'ringing' AND ring_deadline <> '' AND ring_deadline <= ?`, fmtTime(now))
}

// StaleInProgress returns answered/active calls with no activity since cutoff.
func (s *Store) StaleInProgress(ctx context.Context, cutoff time.Time) ([]models.Call, error) {
	return s.queryCalls(ctx, selectCallCols+` FROM voice_calls
		WHERE state IN ('answered','active') AND last_activity_at <= ?`, fmtTime(cutoff))
}

// LiveCalls returns the recipient's ringing or in-progress calls.
func (s *Store) LiveCalls(ctx context.Context, recipient string) ([]models.Call, error) {
	return s.queryCalls(ctx, selectCallCols+` FROM voice_calls
		WHERE recipient = ? AND state IN ('ringing','answered','active')`, recipient)
}

// FindPendingDuplicate returns a scheduled or ringing call from the same agent
// to the same recipient with the same message, if one exists.
func (s *Store) FindPendingDuplicate(ctx context.Context, agent, recipient, message string) (models.Call, error) {
	c, err := scanCall(s.db.QueryRowContext(ctx, selectCallCols+` FROM voice_calls
		WHERE agent = ? AND recipient = ? AND message = ? AND state IN ('scheduled','ringing')
		ORDER BY created_at DESC LIMIT 1`, agent, recipient, message))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Call{}, ErrNotFound
	}
	return c, err
}

// RangSince returns the ring times of the recipient's calls that rang at or
// after since, optionally only those placed by agent; oldest first.
func (s *Store) RangSince(ctx context.Context, recipient, agent string, since time.Time) ([]time.Time, error) {
	q := `SELECT rang_at FROM voice_calls WHERE recipient = ? AND rang_at <> '' AND rang_at >= ?`
	args := []any{recipient, fmtTime(since)}
	if agent != "" {
		q += ` AND agent = ?`
		args = append(args, agent)
	}
	q += ` ORDER BY rang_at ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("rang since: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, parseTime(v))
	}
	return out, rows.Err()
}

// CountPending returns how many scheduled calls an agent has outstanding.
func (s *Store) CountPending(ctx context.Context, agent string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM voice_calls WHERE agent = ? AND state = 'scheduled'`, agent).Scan(&n)
	return n, err
}

// CallUpdate describes a state transition and the fields that change with it.
// Nil pointers leave a field as it is.
type CallUpdate struct {
	To           models.CallState
	Outcome      *models.CallOutcome
	Reason       *string
	RingDeadline *time.Time
	RangAt       *time.Time
	AnsweredAt   *time.Time
	AnsweredBy   *string
	EndedAt      *time.Time
	Actor        string
	Detail       string
}

// TransitionCall moves a call to u.To if the state machine allows it from the
// call's current state, applying u's fields and logging the change, all in one
// transaction. The update is conditional on the state read, so two racing
// transitions cannot both win: the loser gets a *models.TransitionError (or
// ErrConflict). It returns the call as it is after the change.
func (s *Store) TransitionCall(ctx context.Context, id string, u CallUpdate) (models.Call, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return models.Call{}, fmt.Errorf("begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := scanCall(tx.QueryRowContext(ctx, selectCallCols+` FROM voice_calls WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Call{}, ErrNotFound
	}
	if err != nil {
		return models.Call{}, err
	}
	from := c.State
	if err := models.CheckTransition(from, u.To); err != nil {
		return c, err
	}
	now := s.now()
	c.State = u.To
	if u.Outcome != nil {
		c.Outcome = *u.Outcome
	}
	if u.Reason != nil {
		c.Reason = *u.Reason
	}
	if u.RingDeadline != nil {
		c.RingDeadline = u.RingDeadline
	}
	if u.RangAt != nil {
		c.RangAt = u.RangAt
	}
	if u.AnsweredAt != nil {
		c.AnsweredAt = u.AnsweredAt
	}
	if u.AnsweredBy != nil {
		c.AnsweredBy = *u.AnsweredBy
	}
	if u.EndedAt != nil {
		c.EndedAt = u.EndedAt
	}
	c.LastActivityAt = now
	c.UpdatedAt = now
	res, err := tx.ExecContext(ctx, `
		UPDATE voice_calls SET state=?, outcome=?, reason=?, ring_deadline=?, rang_at=?, answered_at=?,
			answered_by=?, ended_at=?, last_activity_at=?, updated_at=?
		WHERE id=? AND state=?`,
		string(c.State), string(c.Outcome), c.Reason, fmtOptTime(c.RingDeadline), fmtOptTime(c.RangAt),
		fmtOptTime(c.AnsweredAt), c.AnsweredBy, fmtOptTime(c.EndedAt), fmtTime(c.LastActivityAt),
		fmtTime(c.UpdatedAt), c.ID, string(from))
	if err != nil {
		return models.Call{}, fmt.Errorf("update call: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return models.Call{}, ErrConflict
	}
	if err := insertLogTx(ctx, tx, models.CallLogEntry{CallID: c.ID, At: now, Kind: "state",
		From: from, To: c.State, Actor: u.Actor, Detail: u.Detail}); err != nil {
		return models.Call{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.Call{}, fmt.Errorf("commit transition: %w", err)
	}
	return c, nil
}

// DeferCall moves a still-scheduled call to a later time (and, when expires
// is non-zero, its expiry) and logs why.
func (s *Store) DeferCall(ctx context.Context, id string, until, expires time.Time, actor, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin defer: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now()
	q := `UPDATE voice_calls SET scheduled_at=?, reason=?, updated_at=?`
	args := []any{fmtTime(until), reason, fmtTime(now)}
	if !expires.IsZero() {
		q += `, expires_at=?`
		args = append(args, fmtTime(expires))
	}
	res, err := tx.ExecContext(ctx, q+` WHERE id=? AND state='scheduled'`, append(args, id)...)
	if err != nil {
		return fmt.Errorf("defer call: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	if err := insertLogTx(ctx, tx, models.CallLogEntry{CallID: id, At: now, Kind: "deferred",
		Actor: actor, Detail: reason + " until " + until.UTC().Format(time.RFC3339)}); err != nil {
		return err
	}
	return tx.Commit()
}

// TouchCall records activity on an in-progress call (keeps the sweeper away).
func (s *Store) TouchCall(ctx context.Context, id string) error {
	now := fmtTime(s.now())
	_, err := s.db.ExecContext(ctx, `UPDATE voice_calls SET last_activity_at=?, updated_at=? WHERE id=?`, now, now, id)
	return err
}

// AppendCallLog records a non-transition step of a call.
func (s *Store) AppendCallLog(ctx context.Context, e models.CallLogEntry) error {
	if e.At.IsZero() {
		e.At = s.now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertLogTx(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

// CallLog returns a call's audit log, oldest first.
func (s *Store) CallLog(ctx context.Context, id string) ([]models.CallLogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, call_id, at, kind, from_state, to_state, actor, detail
		FROM voice_call_log WHERE call_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("call log: %w", err)
	}
	defer rows.Close()
	var out []models.CallLogEntry
	for rows.Next() {
		var e models.CallLogEntry
		var at, from, to string
		if err := rows.Scan(&e.ID, &e.CallID, &at, &e.Kind, &from, &to, &e.Actor, &e.Detail); err != nil {
			return nil, err
		}
		e.At, e.From, e.To = parseTime(at), models.CallState(from), models.CallState(to)
		out = append(out, e)
	}
	return out, rows.Err()
}

func insertLogTx(ctx context.Context, tx *sql.Tx, e models.CallLogEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO voice_call_log (call_id, at, kind, from_state, to_state, actor, detail)
		VALUES (?,?,?,?,?,?,?)`, e.CallID, fmtTime(e.At), e.Kind, string(e.From), string(e.To), e.Actor, truncErr(e.Detail))
	if err != nil {
		return fmt.Errorf("insert call log: %w", err)
	}
	return nil
}

// RecordRing stores the result of pushing a call to one device.
func (s *Store) RecordRing(ctx context.Context, callID, deviceID, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO voice_call_rings (call_id, device_id, at, status, error)
		VALUES (?,?,?,?,?) ON CONFLICT(call_id, device_id) DO UPDATE SET at=excluded.at, status=excluded.status, error=excluded.error`,
		callID, deviceID, fmtTime(s.now()), status, truncErr(errMsg))
	if err != nil {
		return fmt.Errorf("record ring: %w", err)
	}
	return nil
}

// DeviceWasRung reports whether the call was offered to the device.
func (s *Store) DeviceWasRung(ctx context.Context, callID, deviceID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM voice_call_rings WHERE call_id = ? AND device_id = ?`,
		callID, deviceID).Scan(&n)
	return n > 0, err
}

// RingingForDevice returns the ringing calls offered to a device.
func (s *Store) RingingForDevice(ctx context.Context, deviceID string) ([]models.Call, error) {
	return s.queryCalls(ctx, selectCallCols+` FROM voice_calls WHERE state = 'ringing'
		AND id IN (SELECT call_id FROM voice_call_rings WHERE device_id = ?) ORDER BY rang_at ASC`, deviceID)
}

// Turn is one stored conversational exchange.
type Turn struct {
	CallID     string
	Seq        int
	DeviceID   string
	Intent     string
	Transcript string
	Response   string
}

// GetTurn returns a stored turn, or ErrNotFound.
func (s *Store) GetTurn(ctx context.Context, callID string, seq int) (Turn, error) {
	t := Turn{CallID: callID, Seq: seq}
	err := s.db.QueryRowContext(ctx, `SELECT device_id, intent, transcript, response FROM voice_turns
		WHERE call_id = ? AND seq = ?`, callID, seq).Scan(&t.DeviceID, &t.Intent, &t.Transcript, &t.Response)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrNotFound
	}
	return t, err
}

// TurnEffects are the persistent consequences of a turn, applied atomically
// with the turn itself so the reply is only ever sent for committed actions.
type TurnEffects struct {
	Outcome  *models.CallOutcome
	FollowUp *models.Call
	Log      []models.CallLogEntry
}

// RecordTurn stores a turn and its effects in one transaction. If a turn with
// the same (call, seq) exists the stored one is returned with ErrConflict and
// nothing is applied, which makes device retries safe.
func (s *Store) RecordTurn(ctx context.Context, t Turn, fx TurnEffects) (Turn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Turn{}, fmt.Errorf("begin turn: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prev Turn
	err = tx.QueryRowContext(ctx, `SELECT device_id, intent, transcript, response FROM voice_turns
		WHERE call_id = ? AND seq = ?`, t.CallID, t.Seq).Scan(&prev.DeviceID, &prev.Intent, &prev.Transcript, &prev.Response)
	if err == nil {
		prev.CallID, prev.Seq = t.CallID, t.Seq
		return prev, ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Turn{}, err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO voice_turns (call_id, seq, device_id, at, intent, transcript, response)
		VALUES (?,?,?,?,?,?,?)`, t.CallID, t.Seq, t.DeviceID, fmtTime(now), t.Intent, t.Transcript, t.Response); err != nil {
		return Turn{}, fmt.Errorf("insert turn: %w", err)
	}
	if fx.FollowUp != nil {
		if err := insertCallTx(ctx, tx, *fx.FollowUp); err != nil {
			return Turn{}, err
		}
		if err := insertLogTx(ctx, tx, models.CallLogEntry{CallID: fx.FollowUp.ID, At: now, Kind: "created",
			To: fx.FollowUp.State, Actor: "device:" + t.DeviceID, Detail: fx.FollowUp.Reason + " of " + t.CallID}); err != nil {
			return Turn{}, err
		}
	}
	if fx.Outcome != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE voice_calls SET outcome=?, last_activity_at=?, updated_at=? WHERE id=?`,
			string(*fx.Outcome), fmtTime(now), fmtTime(now), t.CallID); err != nil {
			return Turn{}, fmt.Errorf("set outcome: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE voice_calls SET last_activity_at=?, updated_at=? WHERE id=?`,
		fmtTime(now), fmtTime(now), t.CallID); err != nil {
		return Turn{}, fmt.Errorf("touch call: %w", err)
	}
	for _, e := range fx.Log {
		e.At = now
		if err := insertLogTx(ctx, tx, e); err != nil {
			return Turn{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("commit turn: %w", err)
	}
	return t, nil
}

func (s *Store) queryCalls(ctx context.Context, q string, args ...any) ([]models.Call, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query calls: %w", err)
	}
	defer rows.Close()
	var out []models.Call
	for rows.Next() {
		c, err := scanCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCall(sc scanner) (models.Call, error) {
	var (
		c                                                   models.Call
		simulated, test, interactive, userRequested         int
		severity, state, outcome                            string
		scheduled, expires, deadline, rang, answered, ended string
		activity, created, updated                          string
	)
	err := sc.Scan(&c.ID, &c.EventRef, &c.Agent, &c.CallerName, &simulated, &test, &c.Recipient, &c.Type,
		&severity, &c.Title, &c.Message, &c.Details, &interactive, &userRequested, &c.IdempotencyKey, &state,
		&outcome, &c.Reason, &c.Attempt, &c.ParentCallID, &c.Origin, &scheduled, &expires, &deadline,
		&rang, &answered, &c.AnsweredBy, &ended, &activity, &created, &updated)
	if err != nil {
		return models.Call{}, err
	}
	c.Simulated, c.Test, c.Interactive, c.UserRequested = simulated != 0, test != 0, interactive != 0, userRequested != 0
	c.Severity, c.State, c.Outcome = models.Severity(severity), models.CallState(state), models.CallOutcome(outcome)
	c.ScheduledAt, c.ExpiresAt = parseTime(scheduled), parseTime(expires)
	c.RingDeadline, c.RangAt = parseOptTime(deadline), parseOptTime(rang)
	c.AnsweredAt, c.EndedAt = parseOptTime(answered), parseOptTime(ended)
	c.LastActivityAt, c.CreatedAt, c.UpdatedAt = parseTime(activity), parseTime(created), parseTime(updated)
	return c, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func fmtOptTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return fmtTime(*t)
}

func parseOptTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t := parseTime(s)
	return &t
}
