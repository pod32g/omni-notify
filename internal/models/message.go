package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Message is how the producer wants a notification to read. When Text is set,
// every provider sends it as written instead of its default layout (a severity
// tag, the summary and description, then Source/Type/Severity/Event ID/Time and
// every label and annotation as "k: v" lines).
//
// On the wire Message is one flat object: "title" and "text" are for every
// channel, and any other key is an override for the providers of that kind
// ("telegram": {...}), kept as raw JSON for the provider to parse.
type Message struct {
	// Title fills a channel's title slot (an email subject, a Discord embed
	// title, an ntfy title). Empty: the event's title, without a severity tag.
	// Channels with no title slot (Telegram, Matrix, SMS) send Text alone.
	Title string
	// Text is the body, sent as written: plain text, no formatting.
	Text string
	// Channels holds per-channel overrides keyed by provider kind.
	Channels map[string]json.RawMessage
}

// Custom reports whether the producer wrote the text, so providers must not
// add their own layout.
func (m *Message) Custom() bool { return m != nil && m.Text != "" }

// Channel returns the override for a provider kind, or nil.
func (m *Message) Channel(kind string) json.RawMessage {
	if m == nil {
		return nil
	}
	return m.Channels[kind]
}

// MarshalJSON writes the flat form: title, text, then each channel's override.
func (m Message) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	field := func(k string, v []byte) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		b.Write(v)
	}
	if m.Title != "" {
		v, _ := json.Marshal(m.Title)
		field("title", v)
	}
	if m.Text != "" {
		v, _ := json.Marshal(m.Text)
		field("text", v)
	}
	kinds := make([]string, 0, len(m.Channels))
	for k := range m.Channels {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		field(k, m.Channels[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON reads the flat form. Every key other than title and text must
// hold an object: it is a channel override.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = Message{}
	for k, v := range raw {
		switch k {
		case "title":
			if err := json.Unmarshal(v, &m.Title); err != nil {
				return fmt.Errorf("message.title: %w", err)
			}
		case "text":
			if err := json.Unmarshal(v, &m.Text); err != nil {
				return fmt.Errorf("message.text: %w", err)
			}
		default:
			if t := bytes.TrimSpace(v); len(t) == 0 || t[0] != '{' {
				return fmt.Errorf("message.%s must be an object (a channel override)", k)
			}
			if m.Channels == nil {
				m.Channels = map[string]json.RawMessage{}
			}
			m.Channels[k] = append(json.RawMessage(nil), v...)
		}
	}
	return nil
}

// SentMessage remembers a message a provider sent, under a name (ref) a later
// event can use to edit it, reply to it, pin it or react to it.
type SentMessage struct {
	Channel   string // provider kind, "telegram"
	Account   string // the sending account (a Telegram bot's ID)
	Chat      string // the chat it went to
	Ref       string // the producer's name for it (default: the event fingerprint)
	MessageID string // the channel's ID for the message
	Kind      string // what was sent ("text", "media", "rich"); decides how it is edited
	SentAt    time.Time
}
