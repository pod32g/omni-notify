package providers

import (
	"encoding/json"
	"errors"
	"strings"
)

// matrixOverride is an event's message.matrix, in client-server API terms.
type matrixOverride struct {
	Body          string          `json:"body,omitempty"`           // plain text, for clients without HTML
	FormattedBody string          `json:"formatted_body,omitempty"` // HTML (org.matrix.custom.html)
	Msgtype       string          `json:"msgtype,omitempty"`        // m.text, m.notice or m.emote
	Mentions      json.RawMessage `json:"mentions,omitempty"`       // m.mentions: {"user_ids": [...], "room": true}

	Ref     string           `json:"ref,omitempty"`
	ReplyTo string           `json:"reply_to,omitempty"`
	Thread  string           `json:"thread,omitempty"` // ref of the thread's root message
	Edit    string           `json:"edit,omitempty"`
	Pin     bool             `json:"pin,omitempty"`
	Unpin   string           `json:"unpin,omitempty"`
	Delete  string           `json:"delete,omitempty"` // redact
	React   *matrixReactSpec `json:"react,omitempty"`
	Send    *bool            `json:"send,omitempty"` // false: only unpin, delete and react
}

// matrixReactSpec is a reaction (m.annotation): To's message, else the
// edited or replied-to one, else this one.
type matrixReactSpec struct {
	To    string `json:"to,omitempty"`
	Emoji string `json:"emoji"`
}

var matrixMsgtypes = map[string]bool{"m.text": true, "m.notice": true, "m.emote": true}

func parseMatrixOverride(raw json.RawMessage) (matrixOverride, error) {
	var ov matrixOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return matrixOverride{}, err
	}
	if ov.Msgtype != "" && !matrixMsgtypes[ov.Msgtype] {
		return matrixOverride{}, errors.New("msgtype must be m.text, m.notice or m.emote")
	}
	if len(ov.Body)+len(ov.FormattedBody) > 60000 {
		return matrixOverride{}, errors.New("body and formatted_body are over Matrix's 64 KiB event limit")
	}
	if err := checkObject("mentions", ov.Mentions); err != nil {
		return matrixOverride{}, err
	}
	if err := checkRefs(map[string]string{"ref": ov.Ref, "reply_to": ov.ReplyTo, "thread": ov.Thread,
		"edit": ov.Edit, "unpin": ov.Unpin, "delete": ov.Delete}); err != nil {
		return matrixOverride{}, err
	}
	if ov.React != nil && strings.TrimSpace(ov.React.Emoji) == "" {
		return matrixOverride{}, errors.New("react.emoji is required")
	}
	if ov.Send != nil && !*ov.Send {
		if ov.Unpin == "" && ov.Delete == "" && (ov.React == nil || ov.React.To == "") {
			return matrixOverride{}, errNothingToDo
		}
		if ov.Edit != "" {
			return matrixOverride{}, errors.New("send false and edit can't be used together")
		}
	}
	return ov, nil
}
