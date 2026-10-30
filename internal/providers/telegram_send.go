package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/pod32g/omni-notify/internal/models"
)

// What a remembered message is, which decides how it is edited.
const (
	kindText  = "text"
	kindMedia = "media"
	kindRich  = "rich"
)

// tgOutgoing is one notification ready for Telegram: its content, buttons and
// the options every send method takes.
type tgOutgoing struct {
	text            string
	parseMode       string
	entities        json.RawMessage
	rich            json.RawMessage
	media           []tgMediaItem
	captionAbove    bool
	truncateCaption bool // the default layout: cut a long caption, as before
	markup          *tgKeyboard
	linkPreview     json.RawMessage
	options         map[string]any // chat_id, disable_notification, protect_content, ...
	replyTo         int64
}

// tgMediaItem is a media item to send. uploadOnly items (the image_url
// annotation) go out only when Omni-Notify may fetch and upload them.
type tgMediaItem struct {
	tgMediaSpec
	uploadOnly bool
}

// tgSent is the message a send or edit left in the chat.
type tgSent struct {
	id   int64
	kind string
}

var defaultLinkPreview = json.RawMessage(`{"is_disabled":true}`)

// Send delivers one notification. Its message.telegram decides how; without
// one it is message.text (or the default layout) with the annotations' photo
// and buttons. Only the message itself can fail the delivery: pins, reactions
// and deletions are best effort, so a retry never sends it twice.
func (t *telegram) Send(ctx context.Context, msg models.NotificationMessage) error {
	ov, err := parseTelegramOverride(msg.Event.Message.Channel(telegramChannel))
	if err != nil {
		// Checked when the event arrived; one stored before a rule changed
		// still goes out, as if it had no override.
		slog.Warn("telegram: ignoring an invalid override", "event_id", msg.Event.EventID, "err", err)
		ov = tgOverride{}
	}
	refs := t.refsFor(msg)
	out := t.compose(msg, ov)
	if ov.Send != nil && !*ov.Send {
		return t.actions(ctx, refs, ov, tgSent{}, 0, true)
	}

	var target int64 // the message edited or replied to
	var sent tgSent
	edited := false
	if ov.ReplyTo != "" {
		if m, ok := refs.get(ctx, ov.ReplyTo); ok {
			out.replyTo, target = m.id, m.id
		}
	}
	if ov.Edit != "" {
		if m, ok := refs.get(ctx, ov.Edit); ok {
			target = m.id
			sent, edited = t.edit(ctx, out, m)
		}
	}
	if !edited {
		if sent, err = t.deliver(ctx, out); err != nil {
			return err
		}
	}
	refs.put(ctx, refOf(ov.Ref, ov.Edit, msg.Event), sent)
	_ = t.actions(ctx, refs, ov, sent, target, false)
	return nil
}

// compose works out the message: the override's text, else message.text,
// else the default layout; the override's media and buttons, else the
// annotations'; delivery options from the override, else the config.
func (t *telegram) compose(msg models.NotificationMessage, ov tgOverride) tgOutgoing {
	ev := msg.Event
	out := tgOutgoing{
		rich:         ov.Rich,
		captionAbove: ov.ShowCaptionAboveMedia,
		linkPreview:  ov.LinkPreviewOptions,
		options:      map[string]any{"chat_id": t.settings.chatID},
	}
	switch {
	case ov.Text != "":
		out.text, out.parseMode, out.entities = ov.Text, ov.ParseMode, ov.Entities
	case ev.Message.Custom():
		out.text, out.parseMode, out.entities = ev.Message.Text, ov.ParseMode, ov.Entities
	default:
		out.text = subjectLine(ev) + "\n\n" + plainBody(withoutRenderHints(ev))
		out.parseMode = t.settings.parseMode
		out.truncateCaption = true
	}
	if len(out.linkPreview) == 0 {
		out.linkPreview = defaultLinkPreview
	}

	if len(ov.Media) > 0 {
		for _, m := range ov.Media {
			out.media = append(out.media, tgMediaItem{tgMediaSpec: m})
		}
	} else if raw := ev.Annotations["image_url"]; raw != "" && len(t.settings.mediaHosts) > 0 {
		out.media = []tgMediaItem{{tgMediaSpec: tgMediaSpec{Type: "photo", URL: raw}, uploadOnly: true}}
	}
	if ov.Buttons != nil {
		out.markup = t.overrideKeyboard(msg, ov)
	} else {
		out.markup = t.keyboard(msg)
	}

	silent := t.settings.silent[ev.Severity]
	if ov.DisableNotification != nil {
		silent = *ov.DisableNotification
	}
	if silent {
		out.options["disable_notification"] = true
	}
	if ov.ProtectContent {
		out.options["protect_content"] = true
	}
	if ov.MessageEffect != "" {
		out.options["message_effect_id"] = effectIDOf(ov.MessageEffect)
	}
	thread := ov.MessageThreadID
	if thread == 0 {
		thread = t.settings.threadID
	}
	if thread > 0 {
		out.options["message_thread_id"] = thread
	}
	return out
}

// overrideKeyboard builds message.telegram's buttons, row by row. Callback
// buttons need the stored event and an allowed host; others are skipped, and
// so is a row left empty.
func (t *telegram) overrideKeyboard(msg models.NotificationMessage, ov tgOverride) *tgKeyboard {
	var rows [][]tgButton
	callback := 0
	for _, row := range ov.Buttons {
		var btns []tgButton
		for _, b := range row {
			btn := tgButton{
				Text:              truncate(strings.TrimSpace(b.Text), buttonLabelLimit),
				Style:             b.Style,
				IconCustomEmojiID: b.IconCustomEmojiID,
			}
			switch {
			case b.URL != "":
				btn.URL = strings.TrimSpace(b.URL)
			case b.CopyText != "":
				btn.CopyText = &tgCopyText{Text: b.CopyText}
			case b.Disabled:
				btn.Disabled = &struct{}{}
			case b.CallbackURL != "":
				index := callback
				callback++
				if len(t.settings.callbackHosts) == 0 || msg.EventRef <= 0 || !t.settings.callbackHosts.allows(b.CallbackURL) {
					continue
				}
				btn.CallbackData = signCallbackV(t.callbackKey, t.settings.chatID, callbackMessage, msg.EventRef, index)
			default:
				continue
			}
			btns = append(btns, btn)
		}
		if len(btns) > 0 {
			rows = append(rows, btns)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return &tgKeyboard{InlineKeyboard: rows}
}

// deliver sends the message: a rich message, media or an album, falling back
// to plain text when Telegram refuses them or the media cannot be had.
func (t *telegram) deliver(ctx context.Context, out tgOutgoing) (tgSent, error) {
	if len(out.rich) > 0 {
		sent, err := t.sendRich(ctx, out)
		if err == nil {
			return sent, nil
		}
		slog.Warn("telegram: sending text instead of the rich message", "err", err)
	}
	switch {
	case len(out.media) == 1:
		sent, err := t.sendMedia(ctx, out)
		if err == nil {
			return sent, nil
		}
		slog.Warn("telegram: sending without the media", "err", err)
	case len(out.media) > 1:
		sent, err := t.sendAlbum(ctx, out)
		if err == nil {
			return sent, nil
		}
		slog.Warn("telegram: sending without the album", "err", err)
	}
	return t.sendText(ctx, out, out.markup, out.replyTo)
}

// base returns the options every send method takes, with the reply.
func (out tgOutgoing) base(replyTo int64) map[string]any {
	p := make(map[string]any, len(out.options)+4)
	for k, v := range out.options {
		p[k] = v
	}
	if replyTo > 0 {
		p["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	return p
}

// sendText sends the text as a message with markup, a reply to replyTo.
func (t *telegram) sendText(ctx context.Context, out tgOutgoing, markup *tgKeyboard, replyTo int64) (tgSent, error) {
	p := out.base(replyTo)
	setText(p, "text", "entities", out.text, out.parseMode, out.entities, telegramTextLimit)
	p["link_preview_options"] = out.linkPreview
	if markup != nil {
		p["reply_markup"] = markup
	}
	ids, err := t.callDegrading(ctx, "sendMessage", p, nil)
	if err != nil {
		return tgSent{}, err
	}
	return tgSent{id: first(ids), kind: kindText}, nil
}

// sendRich sends message.telegram.rich with sendRichMessage.
func (t *telegram) sendRich(ctx context.Context, out tgOutgoing) (tgSent, error) {
	p := out.base(out.replyTo)
	p["rich_message"] = out.rich
	if out.markup != nil {
		p["reply_markup"] = out.markup
	}
	ids, err := t.callDegrading(ctx, "sendRichMessage", p, nil)
	if err != nil {
		return tgSent{}, err
	}
	return tgSent{id: first(ids), kind: kindRich}, nil
}

// captionFits reports whether the text can be the media's caption. A text too
// long for one goes out as its own message, a reply to the media, unless it
// is the default layout, which is cut as before.
func (out tgOutgoing) captionFits() bool {
	return out.truncateCaption || visibleLen(out.text, out.parseMode) <= telegramCaptionLimit
}

// sendMedia sends one media item with the text as its caption and the
// buttons; when the text does not fit a caption, the media goes first and the
// text with the buttons follows as a reply to it.
func (t *telegram) sendMedia(ctx context.Context, out tgOutgoing) (tgSent, error) {
	item := out.media[0]
	// An upload goes in the multipart field named after the parameter.
	value, file, err := t.mediaInput(ctx, item, item.Type)
	if err != nil {
		return tgSent{}, err
	}
	method := mediaTypes[item.Type]
	p := out.base(out.replyTo)
	var files []tgUpload
	if file != nil {
		files = []tgUpload{*file}
	} else {
		p[item.Type] = value
	}
	fits := out.captionFits()
	if fits {
		setText(p, "caption", "caption_entities", out.text, out.parseMode, out.entities, telegramCaptionLimit)
		if out.markup != nil {
			p["reply_markup"] = out.markup
		}
	}
	if item.HasSpoiler && item.Type != "audio" && item.Type != "document" {
		p["has_spoiler"] = true
	}
	if out.captionAbove && (item.Type == "photo" || item.Type == "video" || item.Type == "animation") {
		p["show_caption_above_media"] = true
	}
	ids, err := t.callDegrading(ctx, method, p, files)
	if err != nil {
		return tgSent{}, err
	}
	sent := tgSent{id: first(ids), kind: kindMedia}
	if fits {
		return sent, nil
	}
	// The media is out: the text must not fail the delivery now (a retry
	// would send the media again), so a failure here is only logged.
	follow, err := t.sendText(ctx, out, out.markup, sent.id)
	if err != nil {
		slog.Warn("telegram: the text after the media failed", "err", err)
		return sent, nil
	}
	return follow, nil
}

// sendAlbum sends 2-10 media items with sendMediaGroup. The text is the first
// item's caption when it fits, the item has none and there are no buttons
// (an album can't have any); otherwise it follows as a reply, with the buttons.
func (t *telegram) sendAlbum(ctx context.Context, out tgOutgoing) (tgSent, error) {
	var files []tgUpload
	media := make([]map[string]any, 0, len(out.media))
	for i, item := range out.media {
		value, file, err := t.mediaInput(ctx, item, fmt.Sprintf("media%d", i))
		if err != nil {
			return tgSent{}, err
		}
		if file != nil {
			files = append(files, *file)
			value = "attach://" + file.field
		}
		m := map[string]any{"type": item.Type, "media": value}
		if item.Caption != "" {
			m["caption"] = truncate(item.Caption, telegramCaptionLimit)
		}
		if item.HasSpoiler && (item.Type == "photo" || item.Type == "video") {
			m["has_spoiler"] = true
		}
		if out.captionAbove && (item.Type == "photo" || item.Type == "video") {
			m["show_caption_above_media"] = true
		}
		media = append(media, m)
	}
	inCaption := out.markup == nil && out.media[0].Caption == "" && out.captionFits()
	if inCaption {
		setText(media[0], "caption", "caption_entities", out.text, out.parseMode, out.entities, telegramCaptionLimit)
	}
	p := out.base(out.replyTo)
	p["media"] = media
	ids, err := t.callDegrading(ctx, "sendMediaGroup", p, files)
	if err != nil {
		return tgSent{}, err
	}
	sent := tgSent{id: first(ids), kind: kindMedia}
	if inCaption {
		return sent, nil
	}
	follow, err := t.sendText(ctx, out, out.markup, sent.id)
	if err != nil {
		slog.Warn("telegram: the text after the album failed", "err", err)
		return sent, nil
	}
	return follow, nil
}

// edit changes a remembered message to this one: its text (or caption) and
// buttons; media are not replaced. It reports false when Telegram would not
// edit it (too old, deleted), so a new message is sent instead.
func (t *telegram) edit(ctx context.Context, out tgOutgoing, target tgSent) (tgSent, bool) {
	p := map[string]any{"chat_id": t.settings.chatID, "message_id": target.id}
	if out.markup != nil {
		p["reply_markup"] = out.markup
	}
	method, kind := "editMessageText", kindText
	switch {
	case target.kind == kindMedia:
		method, kind = "editMessageCaption", kindMedia
		setText(p, "caption", "caption_entities", out.text, out.parseMode, out.entities, telegramCaptionLimit)
		if out.captionAbove {
			p["show_caption_above_media"] = true
		}
	case len(out.rich) > 0:
		p["rich_message"] = out.rich
		kind = kindRich
	default:
		setText(p, "text", "entities", out.text, out.parseMode, out.entities, telegramTextLimit)
		p["link_preview_options"] = out.linkPreview
	}
	_, err := t.callDegrading(ctx, method, p, nil)
	if err != nil && !refusalSays(err, "message is not modified") {
		slog.Warn("telegram: edit failed, sending a new message", "message_id", target.id, "err", err)
		return tgSent{}, false
	}
	return tgSent{id: target.id, kind: kind}, true
}

// actions runs the override's pin, unpin, reaction and deletion. They are
// best effort: refusals (a message too old, a missing right) are logged.
// strict (send false, where they are the delivery) returns errors worth
// retrying (network, rate limit, outage).
func (t *telegram) actions(ctx context.Context, refs tgRefs, ov tgOverride, sent tgSent, target int64, strict bool) error {
	var retry error
	do := func(what, method string, p map[string]any) {
		p["chat_id"] = t.settings.chatID
		if err := t.api.call(ctx, method, p, nil); err != nil {
			slog.Warn("telegram: "+what+" failed", "err", err)
			if !isRefused(err) && retry == nil {
				retry = fmt.Errorf("%s: %w", what, err)
			}
		}
	}
	if ov.Pin && sent.id != 0 {
		do("pin", "pinChatMessage", map[string]any{"message_id": sent.id, "disable_notification": true})
	}
	if ov.Unpin != "" {
		if m, ok := refs.get(ctx, ov.Unpin); ok {
			do("unpin", "unpinChatMessage", map[string]any{"message_id": m.id})
		}
	}
	if r := ov.React; r != nil {
		id := sent.id
		if target != 0 {
			id = target
		}
		if r.To != "" {
			id = 0
			if m, ok := refs.get(ctx, r.To); ok {
				id = m.id
			}
		}
		if id != 0 {
			do("react", "setMessageReaction", map[string]any{
				"message_id": id, "reaction": []any{reactionType(r.Emoji)}, "is_big": r.IsBig,
			})
		}
	}
	if ov.Delete != "" {
		if m, ok := refs.get(ctx, ov.Delete); ok && m.id != sent.id {
			do("delete", "deleteMessage", map[string]any{"message_id": m.id})
			refs.forget(ctx, ov.Delete)
		}
	}
	if strict {
		return retry
	}
	return nil
}

// reactionType is an emoji reaction, or a custom emoji one given by its ID.
func reactionType(emoji string) map[string]any {
	emoji = strings.TrimSpace(emoji)
	if effectID.MatchString(emoji) {
		return map[string]any{"type": "custom_emoji", "custom_emoji_id": emoji}
	}
	return map[string]any{"type": "emoji", "emoji": emoji}
}

// setText puts the text into p under key, with its formatting, cut to limit.
// Formatted text too long is sent as plain text (cutting it could break its
// markup).
func setText(p map[string]any, key, entitiesKey, text, parseMode string, entities json.RawMessage, limit int) {
	if visibleLen(text, parseMode) > limit && (parseMode != "" || len(entities) > 0) {
		text, parseMode, entities = visibleText(text, parseMode), "", nil
	}
	p[key] = truncateUnits(text, limit)
	if parseMode != "" {
		p["parse_mode"] = parseMode
	}
	if len(entities) > 0 {
		p[entitiesKey] = entities
	}
}

// Optional parameters dropped when Telegram refuses a message: a button, an
// effect, a preview, a topic or a reply must never cost it.
var droppable = []string{"reply_markup", "message_effect_id", "link_preview_options", "message_thread_id", "reply_parameters", "protect_content"}

// droppableWords are what Telegram's refusals of those parameters mention.
var droppableWords = []string{"button", "markup", "keyboard", "thread", "topic", "effect", "reply", "preview", "protect"}

// callDegrading calls a send or edit method; when Telegram refuses it (400) it
// tries again without the formatting (if that was the trouble) or without the
// optional parameters, so a message goes out in some form whenever possible.
// The plain text message is the last resort, so it drops them on any refusal;
// other methods (which have a fallback) only on a refusal that names them,
// rather than sending media again that Telegram refused for itself.
func (t *telegram) callDegrading(ctx context.Context, method string, p map[string]any, files []tgUpload) ([]int64, error) {
	lastResort := method == "sendMessage"
	for {
		ids, err := t.post(ctx, method, p, files)
		if err == nil || !isRefused(err) {
			return ids, err
		}
		switch {
		case refusalSays(err, "parse") && hasFormatting(p):
			slog.Warn("telegram: sending without formatting", "method", method, "err", err)
			plainText(p)
		case hasAny(p, droppable) && (lastResort || refusalNames(err, droppableWords)):
			slog.Warn("telegram: sending without buttons and options", "method", method, "err", err)
			for _, k := range droppable {
				delete(p, k)
			}
		default:
			return nil, err
		}
	}
}

func refusalNames(err error, words []string) bool {
	for _, w := range words {
		if refusalSays(err, w) {
			return true
		}
	}
	return false
}

func hasFormatting(p map[string]any) bool {
	return hasAny(p, []string{"parse_mode", "entities", "caption_entities"})
}

func hasAny(p map[string]any, keys []string) bool {
	for _, k := range keys {
		if _, ok := p[k]; ok {
			return true
		}
	}
	return false
}

// plainText turns p's text or caption into plain text.
func plainText(p map[string]any) {
	mode, _ := p["parse_mode"].(string)
	for _, k := range []string{"text", "caption"} {
		if s, ok := p[k].(string); ok {
			p[k] = visibleText(s, mode)
		}
	}
	delete(p, "parse_mode")
	delete(p, "entities")
	delete(p, "caption_entities")
}

// tgUpload is a file uploaded with a send, under a multipart field name.
type tgUpload struct {
	field, filename string
	data            []byte
}

// post calls method with p as JSON, or as multipart form fields when files
// are uploaded (objects JSON-encoded, as the Bot API wants).
func (t *telegram) post(ctx context.Context, method string, p map[string]any, files []tgUpload) ([]int64, error) {
	if len(files) == 0 {
		return t.api.sentJSON(ctx, method, p)
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range p {
		var s string
		switch v := v.(type) {
		case string:
			s = v
		case bool:
			s = strconv.FormatBool(v)
		case int64:
			s = strconv.FormatInt(v, 10)
		case json.RawMessage:
			s = string(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("telegram %s: marshal %s: %w", method, k, err)
			}
			s = string(b)
		}
		_ = w.WriteField(k, s)
	}
	for _, f := range files {
		part, err := w.CreateFormFile(f.field, f.filename)
		if err != nil {
			return nil, fmt.Errorf("telegram %s: %w", method, err)
		}
		if _, err := part.Write(f.data); err != nil {
			return nil, fmt.Errorf("telegram %s: %w", method, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("telegram %s: %w", method, err)
	}
	return t.api.sent(ctx, method, w.FormDataContentType(), &body)
}

func first(ids []int64) int64 {
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

// tgRefs is the provider's refBook, with Telegram's integer message IDs.
type tgRefs struct{ refBook }

func (t *telegram) refsFor(msg models.NotificationMessage) tgRefs {
	return tgRefs{newRefBook(t.store, msg, telegramChannel, t.settings.botID(), t.settings.chatID)}
}

func (r tgRefs) get(ctx context.Context, ref string) (tgSent, bool) {
	m, ok := r.refBook.get(ctx, ref)
	if !ok {
		return tgSent{}, false
	}
	id, err := strconv.ParseInt(m.MessageID, 10, 64)
	if err != nil || id <= 0 {
		return tgSent{}, false
	}
	return tgSent{id: id, kind: m.Kind}, true
}

func (r tgRefs) put(ctx context.Context, ref string, sent tgSent) {
	if sent.id != 0 {
		r.refBook.put(ctx, ref, strconv.FormatInt(sent.id, 10), sent.kind)
	}
}
