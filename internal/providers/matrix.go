package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// matrix sends a message to a Matrix room via the client-server API.
//
// secret: the access token (required).
// config:
//   - homeserver: base URL of the homeserver, e.g. "https://matrix.org" (required)
//   - room_id:    target room ID, e.g. "!abc:hs.tld" (required)
//   - msgtype:    Matrix msgtype (optional, default "m.text")
//
// An event's message.matrix (see matrixOverride) adds HTML, mentions and the
// msgtype, and replies to, threads under, edits, reacts to, pins, unpins or
// redacts earlier messages by ref.
type matrix struct {
	client     *http.Client
	homeserver string
	roomID     string
	msgtype    string
	token      string
	store      MessageStore
}

func newMatrix(client *http.Client, cfg map[string]any, secret string, allowPrivate bool, store MessageStore) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("matrix: access token secret is required")
	}
	homeserver := cfgString(cfg, "homeserver")
	if homeserver == "" {
		return nil, fmt.Errorf("matrix: homeserver is required")
	}
	roomID := cfgString(cfg, "room_id")
	if roomID == "" {
		return nil, fmt.Errorf("matrix: room_id is required")
	}
	if err := validateHTTPURL(homeserver, allowPrivate); err != nil {
		return nil, fmt.Errorf("matrix: %w", err)
	}
	return &matrix{
		client:     client,
		homeserver: strings.TrimRight(homeserver, "/"),
		roomID:     roomID,
		msgtype:    cfgStringDefault(cfg, "msgtype", "m.text"),
		token:      secret,
		store:      store,
	}, nil
}

func (m *matrix) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	ov := overrideOf(ev, "matrix", parseMatrixOverride)
	refs := newRefBook(m.store, msg, "matrix", accountOf(m.homeserver, m.token), m.roomID)
	if ov.Send != nil && !*ov.Send {
		return m.actions(ctx, refs, ov, "", "", true)
	}
	plain := map[string]any{"msgtype": m.msgtype, "body": fullText(ev, "\n\n")}
	content := m.content(ev, ov)

	sent, target := "", "" // the message this event's ref names; the one it replies to or edits
	relates := map[string]any{}
	if root, ok := refs.get(ctx, ov.Thread); ok {
		relates["rel_type"], relates["event_id"] = "m.thread", root.MessageID
		relates["is_falling_back"] = true
		relates["m.in_reply_to"] = map[string]any{"event_id": root.MessageID}
	}
	if r, ok := refs.get(ctx, ov.ReplyTo); ok {
		target = r.MessageID
		relates["m.in_reply_to"] = map[string]any{"event_id": r.MessageID}
		if _, threaded := relates["rel_type"]; threaded {
			relates["is_falling_back"] = false
		}
	}
	if e, ok := refs.get(ctx, ov.Edit); ok {
		// An edit is a new event replacing the original; the ref keeps naming
		// the original, which later edits must point at.
		sent, target = e.MessageID, e.MessageID
		content = matrixEdit(content, e.MessageID)
		plain = matrixEdit(plain, e.MessageID)
	} else if len(relates) > 0 {
		content["m.relates_to"] = relates
		plain["m.relates_to"] = relates
	}

	id, err := m.sendEvent(ctx, "m.room.message", content)
	if isPayloadRefused(err) && !isEmptyOverride(ov) {
		slog.Warn("matrix: sending the plain message instead", "err", err)
		id, err = m.sendEvent(ctx, "m.room.message", plain)
	}
	if err != nil {
		return err
	}
	if sent == "" {
		sent = id
	}
	refs.put(ctx, refOf(ov.Ref, ov.Edit, ev), sent, "")
	_ = m.actions(ctx, refs, ov, sent, target, false)
	return nil
}

// content is the message: the override's body, HTML, msgtype and mentions over
// message.text (or the default layout) and the configured msgtype.
func (m *matrix) content(ev models.Event, ov matrixOverride) map[string]any {
	c := map[string]any{"msgtype": m.msgtype, "body": fullText(ev, "\n\n")}
	if ov.Msgtype != "" {
		c["msgtype"] = ov.Msgtype
	}
	if ov.FormattedBody != "" {
		c["format"] = "org.matrix.custom.html"
		c["formatted_body"] = ov.FormattedBody
		if !ev.Message.Custom() {
			// The plain body is what clients without HTML show.
			c["body"] = visibleText(ov.FormattedBody, "HTML")
		}
	}
	if ov.Body != "" {
		c["body"] = ov.Body
	}
	if len(ov.Mentions) > 0 {
		c["m.mentions"] = ov.Mentions
	}
	return c
}

// matrixEdit turns content into an edit of the event id (m.replace).
func matrixEdit(content map[string]any, id string) map[string]any {
	edit := map[string]any{
		"msgtype":       content["msgtype"],
		"body":          "* " + fmt.Sprint(content["body"]),
		"m.new_content": content,
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": id},
	}
	if fb, ok := content["formatted_body"]; ok {
		edit["format"] = content["format"]
		edit["formatted_body"] = "* " + fmt.Sprint(fb)
	}
	return edit
}

// actions runs the override's pin, unpin, reaction and redaction: best effort,
// except when they are the whole delivery (strict), when failures worth
// retrying are returned.
func (m *matrix) actions(ctx context.Context, refs refBook, ov matrixOverride, sent, target string, strict bool) error {
	var retry error
	note := func(what string, err error) {
		if err == nil {
			return
		}
		slog.Warn("matrix: "+what+" failed", "err", err)
		if retry == nil && !isPayloadRefused(err) && !isNotFound(err) {
			retry = fmt.Errorf("matrix %s: %w", what, err)
		}
	}
	if ov.Pin && sent != "" {
		note("pin", m.setPinned(ctx, sent, true))
	}
	if r, ok := refs.get(ctx, ov.Unpin); ok {
		note("unpin", m.setPinned(ctx, r.MessageID, false))
	}
	if r := ov.React; r != nil {
		id := sent
		if target != "" {
			id = target
		}
		if r.To != "" {
			id = ""
			if t, ok := refs.get(ctx, r.To); ok {
				id = t.MessageID
			}
		}
		if id != "" {
			_, err := m.sendEvent(ctx, "m.reaction", map[string]any{
				"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": id, "key": r.Emoji},
			})
			note("react", err)
		}
	}
	if d, ok := refs.get(ctx, ov.Delete); ok && d.MessageID != sent {
		_, err := m.call(ctx, http.MethodPut, "/redact/"+url.PathEscape(d.MessageID)+"/"+txnID(), map[string]any{})
		note("redact", err)
		if err == nil {
			refs.forget(ctx, ov.Delete)
		}
	}
	if strict {
		return retry
	}
	return nil
}

// setPinned adds the event to the room's pinned events, or removes it.
func (m *matrix) setPinned(ctx context.Context, id string, pinned bool) error {
	var state struct {
		Pinned []string `json:"pinned"`
	}
	answer, err := m.call(ctx, http.MethodGet, "/state/m.room.pinned_events", nil)
	if err != nil && !isNotFound(err) {
		return err
	}
	_ = json.Unmarshal(answer, &state)
	out := make([]string, 0, len(state.Pinned)+1)
	for _, p := range state.Pinned {
		if p != id {
			out = append(out, p)
		}
	}
	if pinned {
		out = append(out, id)
	}
	_, err = m.call(ctx, http.MethodPut, "/state/m.room.pinned_events", map[string]any{"pinned": out})
	return err
}

// sendEvent sends a room event and returns its ID.
func (m *matrix) sendEvent(ctx context.Context, eventType string, content map[string]any) (string, error) {
	answer, err := m.call(ctx, http.MethodPut, "/send/"+url.PathEscape(eventType)+"/"+txnID(), content)
	if err != nil {
		return "", err
	}
	var res struct {
		EventID string `json:"event_id"`
	}
	_ = json.Unmarshal(answer, &res)
	return res.EventID, nil
}

// call makes a client-server API request on the room.
func (m *matrix) call(ctx context.Context, method, path string, payload any) ([]byte, error) {
	endpoint := m.homeserver + "/_matrix/client/v3/rooms/" + url.PathEscape(m.roomID) + path
	headers := map[string]string{"Authorization": "Bearer " + sanitizeHeader(m.token)}
	var answer []byte
	var err error
	if payload == nil {
		answer, err = doRequest(ctx, m.client, method, endpoint, "", nil, headers)
	} else {
		answer, err = sendJSON(ctx, m.client, method, endpoint, payload, headers)
	}
	if err != nil {
		return nil, fmt.Errorf("matrix: %w", err)
	}
	return answer, nil
}

var txnSeq atomic.Int64

// txnID is a transaction ID unique to this process's sends.
func txnID() string {
	return "on" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(txnSeq.Add(1), 36)
}
