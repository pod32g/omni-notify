package providers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/pod32g/omni-notify/internal/models"
)

// An event's message may carry an override per provider kind
// (message.telegram, message.discord, ...): how it reads and behaves on that
// channel, in that channel's own terms. Each kind parses and checks its own;
// overrideValidators lists them, so ValidateMessage can refuse a bad one when
// the event arrives.
var overrideValidators = map[string]func(json.RawMessage) error{
	"telegram":   validatorOf(parseTelegramOverride),
	"discord":    validatorOf(parseDiscordOverride),
	"slack":      validatorOf(parseSlackOverride),
	"teams":      validatorOf(parseTeamsOverride),
	"googlechat": validatorOf(parseGoogleChatOverride),
	"ntfy":       validatorOf(parseNtfyOverride),
	"gotify":     validatorOf(parseGotifyOverride),
	"pushover":   validatorOf(parsePushoverOverride),
	"matrix":     validatorOf(parseMatrixOverride),
	"smtp":       validatorOf(parseSMTPOverride),
	"pagerduty":  validatorOf(parsePagerDutyOverride),
	"opsgenie":   validatorOf(parseOpsgenieOverride),
	"twilio":     validatorOf(parseTwilioOverride),
}

func validatorOf[T any](parse func(json.RawMessage) (T, error)) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		_, err := parse(raw)
		return err
	}
}

// ValidateMessage checks an event's message: each channel override must be for
// a channel that takes one, and valid for it.
func ValidateMessage(m *models.Message) error {
	if m == nil {
		return nil
	}
	for kind, raw := range m.Channels {
		validate, ok := overrideValidators[kind]
		if !ok {
			kinds := make([]string, 0, len(overrideValidators))
			for k := range overrideValidators {
				kinds = append(kinds, k)
			}
			sort.Strings(kinds)
			return fmt.Errorf("message.%s: unknown channel (overrides exist for %s)", kind, strings.Join(kinds, ", "))
		}
		if err := validate(raw); err != nil {
			return fmt.Errorf("message.%s: %w", kind, err)
		}
	}
	return nil
}

// decodeOverride decodes raw into v, refusing unknown fields so a misspelt
// option is an error, not ignored. Empty raw leaves v as it is.
func decodeOverride(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// overrideOf parses kind's override of ev at send time. It was checked when
// the event arrived; one stored before a rule changed is logged and ignored.
func overrideOf[T any](ev models.Event, kind string, parse func(json.RawMessage) (T, error)) T {
	ov, err := parse(ev.Message.Channel(kind))
	if err != nil {
		slog.Warn(kind+": ignoring an invalid override", "event_id", ev.EventID, "err", err)
		var zero T
		return zero
	}
	return ov
}

// isEmptyOverride reports whether an override sets nothing (every field of an
// override is omitempty).
func isEmptyOverride(ov any) bool {
	b, err := json.Marshal(ov)
	return err == nil && string(b) == "{}"
}

// Checks shared by the overrides.

func checkLen(name, s string, max int) error {
	if utf8.RuneCountInString(s) > max {
		return fmt.Errorf("%s is over %d characters", name, max)
	}
	return nil
}

func checkObjects(name string, items []json.RawMessage, max int) error {
	if max > 0 && len(items) > max {
		return fmt.Errorf("%s: at most %d", name, max)
	}
	for i, it := range items {
		if !isJSONObject(it) {
			return fmt.Errorf("%s[%d] must be an object", name, i)
		}
	}
	return nil
}

func checkObject(name string, raw json.RawMessage) error {
	if len(raw) > 0 && !isJSONObject(raw) {
		return fmt.Errorf("%s must be an object", name)
	}
	return nil
}

func checkWebURL(name, raw string) error {
	if raw != "" && !isWebURL(raw) {
		return fmt.Errorf("%s must be an http(s) URL", name)
	}
	return nil
}

func checkRefs(refs map[string]string) error {
	for name, ref := range refs {
		if len(ref) > maxRefLen {
			return fmt.Errorf("%s is over %d bytes", name, maxRefLen)
		}
	}
	return nil
}

var errNothingToDo = errors.New("send false needs something else to do (delete, unpin, react, clear)")

// accountOf names a sending account for refs without exposing its secret:
// a short hash of what identifies it (a webhook URL, a homeserver and token).
func accountOf(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}
