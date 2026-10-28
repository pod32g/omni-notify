package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

const (
	// imageFetchTimeout bounds fetching an image_url, leaving the rest of the
	// send timeout for the upload or the text fallback.
	imageFetchTimeout = 5 * time.Second
	// maxButtonsPerKind caps the link buttons and the callback buttons each.
	maxButtonsPerKind = 8
	// buttonLabelLimit keeps labels short; Telegram cuts long ones anyway.
	buttonLabelLimit = 64
)

// msgLink is one entry of the actions or callbacks annotation.
type msgLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// parseLinks decodes an actions or callbacks annotation. Malformed JSON gives
// no links: a producer's typo costs the buttons, never the message.
func parseLinks(raw string) []msgLink {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var links []msgLink
	if err := json.Unmarshal([]byte(raw), &links); err != nil {
		return nil
	}
	return links
}

// keyboard builds the message's buttons: callback buttons first (they act),
// then link buttons, two to a row. It returns nil when there are none.
func (t *telegram) keyboard(msg models.NotificationMessage) *tgKeyboard {
	ann := msg.Event.Annotations
	var rows [][]tgButton

	// Callback buttons need the stored event to look the action up again when
	// pressed, so test sends (EventRef 0) never get them.
	if len(t.settings.callbackHosts) > 0 && msg.EventRef > 0 {
		var btns []tgButton
		for i, l := range parseLinks(ann["callbacks"]) {
			if len(btns) == maxButtonsPerKind {
				break
			}
			label := strings.TrimSpace(l.Label)
			if label == "" || !t.settings.callbackHosts.allows(l.URL) {
				continue
			}
			btns = append(btns, tgButton{
				Text:         truncate(label, buttonLabelLimit),
				CallbackData: signCallback(t.callbackKey, t.settings.chatID, msg.EventRef, i),
			})
		}
		rows = append(rows, inPairs(btns)...)
	}

	var links []tgButton
	for _, l := range parseLinks(ann["actions"]) {
		if len(links) == maxButtonsPerKind {
			break
		}
		label := strings.TrimSpace(l.Label)
		if label == "" || !isWebURL(l.URL) {
			continue
		}
		links = append(links, tgButton{Text: truncate(label, buttonLabelLimit), URL: strings.TrimSpace(l.URL)})
	}
	rows = append(rows, inPairs(links)...)

	if len(rows) == 0 {
		return nil
	}
	return &tgKeyboard{InlineKeyboard: rows}
}

// inPairs lays buttons out two to a row.
func inPairs(btns []tgButton) [][]tgButton {
	var rows [][]tgButton
	for i := 0; i < len(btns); i += 2 {
		rows = append(rows, btns[i:min(i+2, len(btns))])
	}
	return rows
}

// isWebURL reports whether raw is an absolute http(s) URL with a host.
func isWebURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// sendPhoto fetches the image and sends it with the caption and buttons.
func (t *telegram) sendPhoto(ctx context.Context, imageURL, caption string, markup *tgKeyboard) error {
	img, filename, err := t.fetchImage(ctx, imageURL)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("chat_id", t.settings.chatID)
	_ = w.WriteField("caption", caption)
	if t.settings.parseMode != "" {
		_ = w.WriteField("parse_mode", t.settings.parseMode)
	}
	if markup != nil {
		b, err := json.Marshal(markup)
		if err != nil {
			return fmt.Errorf("telegram sendPhoto: marshal buttons: %w", err)
		}
		_ = w.WriteField("reply_markup", string(b))
	}
	part, err := w.CreateFormFile("photo", filename)
	if err != nil {
		return fmt.Errorf("telegram sendPhoto: %w", err)
	}
	if _, err := part.Write(img); err != nil {
		return fmt.Errorf("telegram sendPhoto: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("telegram sendPhoto: %w", err)
	}
	return t.api.do(ctx, "sendPhoto", w.FormDataContentType(), &body, nil)
}

// photoTypes are the sniffed image types Telegram takes as a photo, with the
// file name each is uploaded under.
var photoTypes = map[string]string{
	"image/jpeg": "image.jpg",
	"image/png":  "image.png",
	"image/webp": "image.webp",
}

// fetchImage downloads imageURL when it is http(s) on an allowed host,
// following redirects only to allowed hosts, and returns at most
// image_max_bytes of a JPEG, PNG or WebP image (judged by its bytes).
func (t *telegram) fetchImage(ctx context.Context, imageURL string) ([]byte, string, error) {
	u, err := url.Parse(strings.TrimSpace(imageURL))
	if err != nil || !t.settings.imageHosts.allowsURL(u) {
		return nil, "", fmt.Errorf("image_url %q is not on an allowed host", imageURL)
	}
	if err := validateHTTPURL(u.String(), t.allowPrivate); err != nil {
		return nil, "", fmt.Errorf("image_url: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, imageFetchTimeout)
	defer cancel()

	client := *t.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		if !t.settings.imageHosts.allowsURL(req.URL) {
			return fmt.Errorf("redirect to host %q is not allowed", req.URL.Hostname())
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("image_url: %w", err)
	}
	req.Header.Set("Accept", "image/jpeg, image/png, image/webp")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch image: status %d", resp.StatusCode)
	}
	limit := t.settings.imageMaxBytes
	if resp.ContentLength > limit {
		return nil, "", fmt.Errorf("fetch image: %d bytes is over the %d-byte limit", resp.ContentLength, limit)
	}
	img, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("fetch image: %w", err)
	}
	if int64(len(img)) > limit {
		return nil, "", fmt.Errorf("fetch image: over the %d-byte limit", limit)
	}
	kind := http.DetectContentType(img)
	filename, ok := photoTypes[kind]
	if !ok {
		return nil, "", fmt.Errorf("fetch image: %s is not a JPEG, PNG or WebP image", kind)
	}
	return img, filename, nil
}
