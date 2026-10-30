package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ntfyOverride is an event's message.ntfy, in ntfy's JSON publishing terms.
// Given, the message is published as JSON to the server's root URL.
type ntfyOverride struct {
	Title    string            `json:"title,omitempty"`
	Message  string            `json:"message,omitempty"`
	Markdown bool              `json:"markdown,omitempty"`
	Priority int               `json:"priority,omitempty"` // 1 (min) to 5 (max)
	Tags     []string          `json:"tags,omitempty"`     // emoji short codes become emoji
	Click    string            `json:"click,omitempty"`
	Attach   string            `json:"attach,omitempty"`
	Filename string            `json:"filename,omitempty"`
	Icon     string            `json:"icon,omitempty"`
	Actions  []json.RawMessage `json:"actions,omitempty"` // view, broadcast, http, copy; at most 3
	Delay    string            `json:"delay,omitempty"`
	Email    string            `json:"email,omitempty"`

	// Refs are ntfy sequence IDs: no store is needed. A ref that is not a
	// valid sequence ID is hashed into one.
	Ref    string `json:"ref,omitempty"`    // publish under this sequence ID
	Edit   string `json:"edit,omitempty"`   // replace that notification
	Clear  string `json:"clear,omitempty"`  // mark that notification read and dismiss it
	Delete string `json:"delete,omitempty"` // delete that notification
	Send   *bool  `json:"send,omitempty"`   // false: only clear or delete
}

var ntfyActionTypes = map[string]bool{"view": true, "broadcast": true, "http": true, "copy": true}

func parseNtfyOverride(raw json.RawMessage) (ntfyOverride, error) {
	var ov ntfyOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return ntfyOverride{}, err
	}
	if ov.Priority != 0 && (ov.Priority < 1 || ov.Priority > 5) {
		return ntfyOverride{}, errors.New("priority must be between 1 and 5")
	}
	if len(ov.Actions) > 3 {
		return ntfyOverride{}, errors.New("actions: at most 3")
	}
	for i, a := range ov.Actions {
		var act struct {
			Action string `json:"action"`
			Label  string `json:"label"`
		}
		if json.Unmarshal(a, &act) != nil || !ntfyActionTypes[act.Action] || strings.TrimSpace(act.Label) == "" {
			return ntfyOverride{}, fmt.Errorf("actions[%d] needs an action (view, broadcast, http or copy) and a label", i)
		}
	}
	// click may also be mailto:, geo: or an app link; attach and icon are fetched.
	if ov.Click != "" && !strings.Contains(ov.Click, ":") {
		return ntfyOverride{}, errors.New("click must be a URL")
	}
	for name, u := range map[string]string{"attach": ov.Attach, "icon": ov.Icon} {
		if err := checkWebURL(name, u); err != nil {
			return ntfyOverride{}, err
		}
	}
	if err := checkRefs(map[string]string{"ref": ov.Ref, "edit": ov.Edit, "clear": ov.Clear, "delete": ov.Delete}); err != nil {
		return ntfyOverride{}, err
	}
	if ov.Ref != "" && ov.Edit != "" && ov.Ref != ov.Edit {
		return ntfyOverride{}, errors.New("ref and edit name the notification's sequence ID: give one, or the same")
	}
	if ov.Send != nil && !*ov.Send && ov.Clear == "" && ov.Delete == "" {
		return ntfyOverride{}, errNothingToDo
	}
	return ov, nil
}

var ntfySequenceID = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

// ntfySID turns a ref into an ntfy sequence ID: itself when valid, else a hash.
func ntfySID(ref string) string {
	if ref == "" || ntfySequenceID.MatchString(ref) {
		return ref
	}
	sum := sha256.Sum256([]byte(ref))
	return "r" + hex.EncodeToString(sum[:16])
}

// gotifyOverride is an event's message.gotify. Extras are Gotify's message
// extras, passed through; markdown, click_url, big_image_url and intent_url
// are shorthands for the documented ones.
type gotifyOverride struct {
	Title       string          `json:"title,omitempty"`
	Message     string          `json:"message,omitempty"`
	Priority    *int            `json:"priority,omitempty"`
	Markdown    bool            `json:"markdown,omitempty"`      // client::display contentType text/markdown
	ClickURL    string          `json:"click_url,omitempty"`     // client::notification click.url
	BigImageURL string          `json:"big_image_url,omitempty"` // client::notification bigImageUrl
	IntentURL   string          `json:"intent_url,omitempty"`    // android::action onReceive.intentUrl
	Extras      json.RawMessage `json:"extras,omitempty"`
}

func parseGotifyOverride(raw json.RawMessage) (gotifyOverride, error) {
	var ov gotifyOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return gotifyOverride{}, err
	}
	if err := checkObject("extras", ov.Extras); err != nil {
		return gotifyOverride{}, err
	}
	if err := checkWebURL("big_image_url", ov.BigImageURL); err != nil {
		return gotifyOverride{}, err
	}
	return ov, nil
}

// extras merges the shorthands into the given extras (which win).
func (ov gotifyOverride) extras() map[string]any {
	out := map[string]any{}
	if ov.Markdown {
		out["client::display"] = map[string]any{"contentType": "text/markdown"}
	}
	notification := map[string]any{}
	if ov.ClickURL != "" {
		notification["click"] = map[string]any{"url": ov.ClickURL}
	}
	if ov.BigImageURL != "" {
		notification["bigImageUrl"] = ov.BigImageURL
	}
	if len(notification) > 0 {
		out["client::notification"] = notification
	}
	if ov.IntentURL != "" {
		out["android::action"] = map[string]any{"onReceive": map[string]any{"intentUrl": ov.IntentURL}}
	}
	var given map[string]any
	if json.Unmarshal(ov.Extras, &given) == nil {
		for k, v := range given {
			out[k] = v
		}
	}
	return out
}

// pushoverOverride is an event's message.pushover, in Pushover's message API
// terms.
type pushoverOverride struct {
	Title     string `json:"title,omitempty"`
	Message   string `json:"message,omitempty"`
	HTML      bool   `json:"html,omitempty"`
	Monospace bool   `json:"monospace,omitempty"`
	URL       string `json:"url,omitempty"`
	URLTitle  string `json:"url_title,omitempty"`
	Priority  *int   `json:"priority,omitempty"` // -2 to 2; 2 needs retry and expire
	Retry     int    `json:"retry,omitempty"`
	Expire    int    `json:"expire,omitempty"`
	Sound     string `json:"sound,omitempty"`
	Device    string `json:"device,omitempty"`
	TTL       int    `json:"ttl,omitempty"`
	Tags      string `json:"tags,omitempty"` // receipt tags, to cancel emergency retries by
}

func parsePushoverOverride(raw json.RawMessage) (pushoverOverride, error) {
	var ov pushoverOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return pushoverOverride{}, err
	}
	for name, l := range map[string][2]any{"title": {ov.Title, 250}, "message": {ov.Message, 1024}, "url": {ov.URL, 512}, "url_title": {ov.URLTitle, 100}} {
		if err := checkLen(name, l[0].(string), l[1].(int)); err != nil {
			return pushoverOverride{}, err
		}
	}
	if ov.HTML && ov.Monospace {
		return pushoverOverride{}, errors.New("html and monospace can't be used together")
	}
	if ov.Priority != nil {
		switch p := *ov.Priority; {
		case p < -2 || p > 2:
			return pushoverOverride{}, errors.New("priority must be between -2 and 2")
		case p == 2 && (ov.Retry < 30 || ov.Expire <= 0 || ov.Expire > 10800):
			return pushoverOverride{}, errors.New("priority 2 needs retry (at least 30 seconds) and expire (at most 10800 seconds)")
		}
	}
	if ov.TTL < 0 {
		return pushoverOverride{}, errors.New("ttl must be positive")
	}
	return ov, nil
}
