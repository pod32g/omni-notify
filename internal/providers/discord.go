package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/pod32g/omni-notify/internal/models"
)

// discord posts rich embeds to a Discord incoming webhook.
//
// secret: the webhook URL (required).
// config: username (optional), avatar_url (optional).
//
// An event's message.discord (see discordOverride) replaces the embed with the
// producer's content, embeds, components or poll, and can edit or delete an
// earlier message by ref. Messages are sent with ?wait=true so their IDs can
// be remembered.
type discord struct {
	client    *http.Client
	url       string
	username  string
	avatarURL string
	store     MessageStore
}

func newDiscord(client *http.Client, cfg map[string]any, secret string, allowPrivate bool, store MessageStore) (Provider, error) {
	if secret == "" {
		return nil, fmt.Errorf("discord: webhook URL secret is required")
	}
	if err := validateHTTPURL(secret, allowPrivate); err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	return &discord{
		client:    client,
		url:       secret,
		username:  cfgString(cfg, "username"),
		avatarURL: cfgString(cfg, "avatar_url"),
		store:     store,
	}, nil
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

func (d *discord) Send(ctx context.Context, msg models.NotificationMessage) error {
	ov := overrideOf(msg.Event, "discord", parseDiscordOverride)
	refs := newRefBook(d.store, msg, "discord", accountOf(d.url), ov.ThreadID)
	if ov.Send != nil && !*ov.Send {
		return d.remove(ctx, refs, ov.Delete, ov.ThreadID, true)
	}
	query := url.Values{}
	if ov.ThreadID != "" {
		query.Set("thread_id", ov.ThreadID)
	}
	if len(ov.Components) > 0 {
		query.Set("with_components", "true")
	}

	id := ""
	if target, ok := refs.get(ctx, ov.Edit); ok {
		var err error
		id, err = d.edit(ctx, target.MessageID, query, d.payload(msg, ov), d.payload(msg, discordOverride{}))
		if err != nil {
			slog.Warn("discord: edit failed, sending a new message", "message_id", target.MessageID, "err", err)
		}
	}
	if id == "" {
		rich, plain := d.payload(msg, ov), d.payload(msg, discordOverride{})
		answer, err := d.call(ctx, http.MethodPost, "", withWait(query), rich)
		if isPayloadRefused(err) && !isEmptyOverride(ov) {
			// A refused embed, component or poll must not cost the message.
			slog.Warn("discord: sending the plain message instead", "err", err)
			answer, err = d.call(ctx, http.MethodPost, "", withWait(nil), plain)
		}
		if err != nil {
			return err
		}
		id = messageIDOf(answer)
	}
	refs.put(ctx, refOf(ov.Ref, ov.Edit, msg.Event), id, "")
	_ = d.remove(ctx, refs, ov.Delete, ov.ThreadID, false)
	return nil
}

// edit replaces a message's content, embeds and components, plainly if the
// rich version is refused. It returns the message ID, "" if it was not edited.
func (d *discord) edit(ctx context.Context, id string, query url.Values, rich, plain map[string]any) (string, error) {
	for k := range rich {
		if k == "username" || k == "avatar_url" || k == "tts" || k == "thread_name" || k == "applied_tags" {
			delete(rich, k) // set on a message's creation only
			delete(plain, k)
		}
	}
	// A field left out keeps its old value: clear the ones this edit doesn't set.
	for _, k := range []string{"content", "embeds", "components"} {
		if _, ok := rich[k]; !ok {
			rich[k] = nil
		}
		if _, ok := plain[k]; !ok {
			plain[k] = nil
		}
	}
	_, err := d.call(ctx, http.MethodPatch, "/messages/"+url.PathEscape(id), query, rich)
	if isPayloadRefused(err) {
		_, err = d.call(ctx, http.MethodPatch, "/messages/"+url.PathEscape(id), query, plain)
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// remove deletes the message remembered as ref. Best effort, except when it is
// the whole delivery (strict): then failures worth retrying are returned.
func (d *discord) remove(ctx context.Context, refs refBook, ref, threadID string, strict bool) error {
	m, ok := refs.get(ctx, ref)
	if !ok {
		return nil
	}
	query := url.Values{}
	if threadID != "" {
		query.Set("thread_id", threadID)
	}
	_, err := d.call(ctx, http.MethodDelete, "/messages/"+url.PathEscape(m.MessageID), query, nil)
	if err == nil || isNotFound(err) {
		refs.forget(ctx, ref)
		return nil
	}
	slog.Warn("discord: delete failed", "message_id", m.MessageID, "err", err)
	if strict && !isPayloadRefused(err) {
		return err
	}
	return nil
}

// call sends payload to the webhook URL plus path and query.
func (d *discord) call(ctx context.Context, method, path string, query url.Values, payload map[string]any) ([]byte, error) {
	u, err := url.Parse(d.url)
	if err != nil {
		return nil, fmt.Errorf("discord: webhook URL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	q := u.Query()
	for k, vs := range query {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	if payload == nil {
		return doRequest(ctx, d.client, method, u.String(), "", nil, nil)
	}
	return sendJSON(ctx, d.client, method, u.String(), payload, nil)
}

func withWait(q url.Values) url.Values {
	out := url.Values{"wait": {"true"}}
	for k, vs := range q {
		out[k] = vs
	}
	return out
}

// messageIDOf reads a message's ID from an answer ("" if it has none).
func messageIDOf(answer []byte) string {
	var m struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(answer, &m)
	return m.ID
}

// payload builds the webhook body: the producer's content, embeds, components
// or poll when the override has any, else the default embed (headline,
// message.text or the summary, fields, colour), with the override's options.
func (d *discord) payload(msg models.NotificationMessage, ov discordOverride) map[string]any {
	ev := msg.Event
	p := map[string]any{}
	if ov.hasContent() {
		if ov.Content != "" {
			p["content"] = ov.Content
		}
		if len(ov.Embeds) > 0 {
			p["embeds"] = ov.Embeds
		}
		if len(ov.Components) > 0 {
			p["components"] = ov.Components
		}
		if len(ov.Poll) > 0 {
			p["poll"] = ov.Poll
		}
	} else {
		embed := discordEmbed{
			Title:       truncate(headline(ev), 256),
			Description: truncate(summaryText(ev, "\n\n"), 4000),
			Color:       statusColorHex(ev),
			Timestamp:   ev.Timestamp.UTC().Format("2006-01-02T15:04:05Z07:00"),
		}
		for _, kv := range detailFields(ev) {
			embed.Fields = append(embed.Fields, discordEmbedField{
				Name:   truncate(kv[0], 256),
				Value:  truncate(kv[1], 1024),
				Inline: true,
			})
		}
		p["embeds"] = []discordEmbed{embed}
		if msg.Test {
			p["content"] = "Omni-Notify test notification"
		}
	}
	username, avatar := d.username, d.avatarURL
	if ov.Username != "" {
		username = ov.Username
	}
	if ov.AvatarURL != "" {
		avatar = ov.AvatarURL
	}
	if username != "" {
		p["username"] = username
	}
	if avatar != "" {
		p["avatar_url"] = avatar
	}
	if ov.TTS {
		p["tts"] = true
	}
	if len(ov.AllowedMentions) > 0 {
		p["allowed_mentions"] = ov.AllowedMentions
	}
	if flags := discordFlagBits(ov.Flags); flags != 0 {
		p["flags"] = flags
	}
	if ov.ThreadName != "" {
		p["thread_name"] = ov.ThreadName
	}
	if len(ov.AppliedTags) > 0 {
		p["applied_tags"] = ov.AppliedTags
	}
	return p
}

// truncate limits s to max characters (runes), appending an ellipsis when
// shortened. It cuts on rune boundaries so the result is always valid UTF-8
// (Discord/Slack reject invalid UTF-8), and treats max as a character count to
// match the providers' documented character limits.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	keep := max - 1 // reserve one rune for the ellipsis
	count := 0
	for i := range s {
		if count == keep {
			return s[:i] + "…"
		}
		count++
	}
	return s
}
