package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// tgOverride is an event's message.telegram: how the producer wants it to read
// and behave in Telegram. Field names follow the Bot API where there is one.
// Every field is optional; what is left out falls back to message.text, the
// annotations' render hints and the provider's config.
type tgOverride struct {
	// Content. Text is the message text, or the caption when media is sent.
	Text      string          `json:"text,omitempty"`
	ParseMode string          `json:"parse_mode,omitempty"` // HTML, MarkdownV2 or Markdown
	Entities  json.RawMessage `json:"entities,omitempty"`   // MessageEntity list, instead of parse_mode
	// Rich is an InputRichMessage (markdown, html or blocks) sent with
	// sendRichMessage: headings, lists, tables, details, media. Text (or
	// message.text, or the default layout) is the fallback if Telegram refuses it.
	Rich json.RawMessage `json:"rich,omitempty"`
	// Media is one item (sendPhoto, sendVideo, ...) or an album of 2-10.
	Media                 []tgMediaSpec `json:"media,omitempty"`
	ShowCaptionAboveMedia bool          `json:"show_caption_above_media,omitempty"`
	// Buttons are the inline keyboard, row by row.
	Buttons [][]tgButtonSpec `json:"buttons,omitempty"`

	// Delivery options.
	DisableNotification *bool           `json:"disable_notification,omitempty"`
	ProtectContent      bool            `json:"protect_content,omitempty"`
	MessageEffect       string          `json:"message_effect,omitempty"` // a name, its emoji or an effect ID
	LinkPreviewOptions  json.RawMessage `json:"link_preview_options,omitempty"`
	MessageThreadID     int64           `json:"message_thread_id,omitempty"`

	// Earlier messages, by ref. A message is remembered under Ref, else the
	// ref it edits, else its event's fingerprint.
	Ref     string       `json:"ref,omitempty"`
	ReplyTo string       `json:"reply_to,omitempty"` // send as a reply to this ref
	Edit    string       `json:"edit,omitempty"`     // edit this ref instead of sending
	Pin     bool         `json:"pin,omitempty"`      // pin the message sent or edited
	Unpin   string       `json:"unpin,omitempty"`    // unpin this ref
	Delete  string       `json:"delete,omitempty"`   // delete this ref
	React   *tgReactSpec `json:"react,omitempty"`    // set a reaction
	// Send false sends no message: only unpin, delete and react run.
	Send *bool `json:"send,omitempty"`
}

// tgMediaSpec is one media item. URL is fetched by Omni-Notify and uploaded
// when its host is in media_hosts, else handed to Telegram to fetch; a value
// that is not a URL is a Telegram file_id.
type tgMediaSpec struct {
	Type       string `json:"type"` // photo, video, animation, audio or document
	URL        string `json:"url"`
	Caption    string `json:"caption,omitempty"` // album items after the first
	HasSpoiler bool   `json:"has_spoiler,omitempty"`
}

// tgButtonSpec is one inline keyboard button: a link (url), an action
// Omni-Notify runs when it is pressed (callback_url, POSTed), text copied to
// the clipboard (copy_text), or a disabled label.
type tgButtonSpec struct {
	Text              string `json:"text"`
	URL               string `json:"url,omitempty"`
	CallbackURL       string `json:"callback_url,omitempty"`
	CopyText          string `json:"copy_text,omitempty"`
	Disabled          bool   `json:"disabled,omitempty"`
	Style             string `json:"style,omitempty"` // danger, success or primary
	IconCustomEmojiID string `json:"icon_custom_emoji_id,omitempty"`
}

// tgReactSpec sets a reaction on a message: To's, else the edited or
// replied-to message, else the one just sent.
type tgReactSpec struct {
	To    string `json:"to,omitempty"`
	Emoji string `json:"emoji"`
	IsBig bool   `json:"is_big,omitempty"`
}

// Telegram's limits used in validation.
const (
	maxAlbumItems     = 10
	maxButtonsPerRow  = 8
	maxButtons        = 100
	maxCopyText       = 256
	maxRefLen         = 256
	maxOverrideText   = 32768 // a rich message's limit; text over 4096 is cut at send
	telegramChannel   = "telegram"
	defaultMediaBytes = 20 << 20
	maxMediaBytes     = 50 << 20 // Telegram's upload limit for bots
)

var (
	parseModes = map[string]string{"html": "HTML", "markdownv2": "MarkdownV2", "markdown": "Markdown"}
	mediaTypes = map[string]string{
		"photo": "sendPhoto", "video": "sendVideo", "animation": "sendAnimation",
		"audio": "sendAudio", "document": "sendDocument",
	}
	buttonStyles = map[string]bool{"danger": true, "success": true, "primary": true}
	// messageEffects names Telegram's free message effects (private chats only).
	messageEffects = map[string]string{
		"fire": "5104841245755180586", "🔥": "5104841245755180586",
		"like": "5107584321108051014", "👍": "5107584321108051014",
		"dislike": "5104858069142078462", "👎": "5104858069142078462",
		"heart": "5159385139981059251", "❤️": "5159385139981059251",
		"party": "5046509860389126442", "🎉": "5046509860389126442",
		"poop": "5046589136895476101", "💩": "5046589136895476101",
	}
	effectID = regexp.MustCompile(`^[0-9]{1,24}$`)
)

// parseTelegramOverride decodes and checks message.telegram. Unknown fields
// are errors, so a misspelt option is not silently ignored. nil raw gives an
// empty override.
func parseTelegramOverride(raw json.RawMessage) (tgOverride, error) {
	var ov tgOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return tgOverride{}, err
	}
	return ov, ov.validate()
}

func (ov *tgOverride) validate() error {
	if ov.ParseMode != "" {
		canon, ok := parseModes[strings.ToLower(ov.ParseMode)]
		if !ok {
			return fmt.Errorf("parse_mode must be HTML, MarkdownV2 or Markdown")
		}
		ov.ParseMode = canon
	}
	if len(ov.Entities) > 0 {
		if ov.ParseMode != "" {
			return errors.New("entities and parse_mode can't be used together")
		}
		if !isJSONArray(ov.Entities) {
			return errors.New("entities must be a list of MessageEntity")
		}
	}
	if utf8.RuneCountInString(ov.Text) > maxOverrideText {
		return fmt.Errorf("text is over %d characters", maxOverrideText)
	}
	if len(ov.Rich) > 0 {
		if err := validateRich(ov.Rich); err != nil {
			return fmt.Errorf("rich: %w", err)
		}
		if len(ov.Media) > 0 {
			return errors.New("rich and media can't be used together (a rich message carries its own media)")
		}
	}
	if err := validateMedia(ov.Media); err != nil {
		return err
	}
	if err := validateButtons(ov.Buttons); err != nil {
		return err
	}
	if ov.MessageEffect != "" {
		if _, ok := messageEffects[strings.ToLower(ov.MessageEffect)]; !ok && !effectID.MatchString(ov.MessageEffect) {
			return fmt.Errorf("message_effect %q: use fire, like, dislike, heart, party, poop or an effect ID", ov.MessageEffect)
		}
	}
	if len(ov.LinkPreviewOptions) > 0 && !isJSONObject(ov.LinkPreviewOptions) {
		return errors.New("link_preview_options must be an object")
	}
	if ov.MessageThreadID < 0 {
		return errors.New("message_thread_id must be positive")
	}
	for name, ref := range map[string]string{"ref": ov.Ref, "reply_to": ov.ReplyTo, "edit": ov.Edit, "unpin": ov.Unpin, "delete": ov.Delete} {
		if len(ref) > maxRefLen {
			return fmt.Errorf("%s is over %d bytes", name, maxRefLen)
		}
	}
	if ov.React != nil {
		if strings.TrimSpace(ov.React.Emoji) == "" {
			return errors.New("react.emoji is required")
		}
		if len(ov.React.To) > maxRefLen {
			return fmt.Errorf("react.to is over %d bytes", maxRefLen)
		}
	}
	if ov.Send != nil && !*ov.Send {
		if ov.Unpin == "" && ov.Delete == "" && (ov.React == nil || ov.React.To == "") {
			return errors.New("send false needs unpin, delete or react.to: there is nothing else to do")
		}
		if ov.Edit != "" {
			return errors.New("send false and edit can't be used together")
		}
	}
	return nil
}

// validateRich checks an InputRichMessage holds exactly one of markdown, html
// and blocks; Telegram checks the rest.
func validateRich(raw json.RawMessage) error {
	var rich map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rich); err != nil {
		return errors.New("must be an InputRichMessage object")
	}
	n := 0
	for _, k := range []string{"markdown", "html", "blocks"} {
		if v, ok := rich[k]; ok && string(v) != "null" && string(v) != `""` {
			n++
		}
	}
	if n != 1 {
		return errors.New("needs exactly one of markdown, html and blocks")
	}
	return nil
}

func validateMedia(media []tgMediaSpec) error {
	if len(media) > maxAlbumItems {
		return fmt.Errorf("media: at most %d items", maxAlbumItems)
	}
	kinds := map[string]bool{}
	for i, m := range media {
		if _, ok := mediaTypes[m.Type]; !ok {
			return fmt.Errorf("media[%d].type must be photo, video, animation, audio or document", i)
		}
		if strings.TrimSpace(m.URL) == "" {
			return fmt.Errorf("media[%d].url is required", i)
		}
		if strings.Contains(m.URL, "://") && !isWebURL(m.URL) {
			return fmt.Errorf("media[%d].url must be http(s) or a Telegram file_id", i)
		}
		kinds[m.Type] = true
	}
	if len(media) > 1 {
		// sendMediaGroup's rules: photos and videos mix; audio and documents
		// only go with their own kind; animations can't be in an album.
		switch {
		case kinds["animation"]:
			return errors.New("media: an animation can't be part of an album")
		case kinds["audio"] && len(kinds) > 1, kinds["document"] && len(kinds) > 1:
			return errors.New("media: audio and documents can only be grouped with their own kind")
		}
	}
	return nil
}

func validateButtons(rows [][]tgButtonSpec) error {
	total := 0
	for r, row := range rows {
		if len(row) == 0 || len(row) > maxButtonsPerRow {
			return fmt.Errorf("buttons[%d]: a row holds 1 to %d buttons", r, maxButtonsPerRow)
		}
		for c, b := range row {
			total++
			where := fmt.Sprintf("buttons[%d][%d]", r, c)
			if strings.TrimSpace(b.Text) == "" {
				return fmt.Errorf("%s.text is required", where)
			}
			kinds := 0
			for _, set := range []bool{b.URL != "", b.CallbackURL != "", b.CopyText != "", b.Disabled} {
				if set {
					kinds++
				}
			}
			if kinds != 1 {
				return fmt.Errorf("%s needs exactly one of url, callback_url, copy_text and disabled", where)
			}
			if b.URL != "" && !isWebURL(b.URL) && !strings.HasPrefix(b.URL, "tg://") {
				return fmt.Errorf("%s.url must be http(s) or tg://", where)
			}
			if b.CallbackURL != "" && !isWebURL(b.CallbackURL) {
				return fmt.Errorf("%s.callback_url must be http(s)", where)
			}
			if utf8.RuneCountInString(b.CopyText) > maxCopyText {
				return fmt.Errorf("%s.copy_text is over %d characters", where, maxCopyText)
			}
			if b.Style != "" && !buttonStyles[b.Style] {
				return fmt.Errorf("%s.style must be danger, success or primary", where)
			}
		}
	}
	if total > maxButtons {
		return fmt.Errorf("buttons: at most %d", maxButtons)
	}
	return nil
}

// callbackSpecs lists the override's callback buttons in keyboard order; a
// button's index in this list is what its callback_data names.
func (ov tgOverride) callbackSpecs() []tgButtonSpec {
	var out []tgButtonSpec
	for _, row := range ov.Buttons {
		for _, b := range row {
			if b.CallbackURL != "" {
				out = append(out, b)
			}
		}
	}
	return out
}

// effectIDOf resolves a message_effect name or emoji to its ID.
func effectIDOf(name string) string {
	if id, ok := messageEffects[strings.ToLower(name)]; ok {
		return id
	}
	return name
}

func isJSONArray(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '['
}

func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// visibleText approximates text as Telegram counts it after parsing: tags and
// entities of HTML resolved; other modes are counted as written (an
// overestimate, which only makes a long caption go out as its own message).
func visibleText(text, parseMode string) string {
	if parseMode != "HTML" {
		return text
	}
	return html.UnescapeString(htmlTag.ReplaceAllString(text, ""))
}

// visibleLen is the length Telegram's limits apply to, in UTF-16 code units.
func visibleLen(text, parseMode string) int {
	n := 0
	for _, r := range visibleText(text, parseMode) {
		n += utf16Len(r)
	}
	return n
}

func utf16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// truncateUnits limits s to max UTF-16 code units (Telegram's measure),
// ending in an ellipsis when shortened.
func truncateUnits(s string, max int) string {
	n := 0
	for _, r := range s {
		n += utf16Len(r)
	}
	if n <= max {
		return s
	}
	n = 0
	for i, r := range s {
		if n+utf16Len(r) > max-1 { // leave room for the ellipsis
			return s[:i] + "…"
		}
		n += utf16Len(r)
	}
	return s
}

// isFileID reports whether a media value is a Telegram file_id, not a URL.
func isFileID(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err != nil || u.Scheme == ""
}
