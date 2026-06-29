package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
type matrix struct {
	client     *http.Client
	homeserver string
	roomID     string
	msgtype    string
	token      string
}

func newMatrix(client *http.Client, cfg map[string]any, secret string, allowPrivate bool) (Provider, error) {
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
		homeserver: homeserver,
		roomID:     roomID,
		msgtype:    cfgStringDefault(cfg, "msgtype", "m.text"),
		token:      secret,
	}, nil
}

type matrixMessage struct {
	Msgtype string `json:"msgtype"`
	Body    string `json:"body"`
}

func (m *matrix) Send(ctx context.Context, msg models.NotificationMessage) error {
	ev := msg.Event
	body := subjectLine(ev) + "\n\n" + plainBody(ev)

	payload := matrixMessage{
		Msgtype: m.msgtype,
		Body:    body,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("matrix: marshal: %w", err)
	}

	txnID := strconv.FormatInt(time.Now().UnixNano(), 10)
	endpoint := strings.TrimRight(m.homeserver, "/") +
		"/_matrix/client/v3/rooms/" + url.PathEscape(m.roomID) +
		"/send/m.room.message/" + txnID

	headers := map[string]string{
		"Authorization": "Bearer " + sanitizeHeader(m.token),
	}
	return postBytes(ctx, m.client, http.MethodPut, endpoint, "application/json", raw, headers)
}
