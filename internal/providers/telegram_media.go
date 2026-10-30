package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
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

// mediaInput is how a media item is given to Telegram: a file_id or a URL as
// is, or, for a URL on media_hosts, the file itself, fetched here and uploaded
// under field. uploadOnly items not on media_hosts are refused.
func (t *telegram) mediaInput(ctx context.Context, item tgMediaItem, field string) (string, *tgUpload, error) {
	raw := strings.TrimSpace(item.URL)
	if isFileID(raw) {
		if item.uploadOnly {
			return "", nil, fmt.Errorf("image_url %q is not a URL", raw)
		}
		return raw, nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, fmt.Errorf("media url %q: %w", raw, err)
	}
	if !t.settings.mediaHosts.allowsURL(u) {
		if item.uploadOnly || !isWebURL(raw) {
			return "", nil, fmt.Errorf("media url %q is not on an allowed host", raw)
		}
		return raw, nil, nil // Telegram fetches it
	}
	data, filename, err := t.fetchMedia(ctx, u, item.Type)
	if err != nil {
		return "", nil, err
	}
	return "", &tgUpload{field: field, filename: filename, data: data}, nil
}

// photoTypes are the sniffed image types Telegram takes as a photo, with the
// file name each is uploaded under.
var photoTypes = map[string]string{
	"image/jpeg": "image.jpg",
	"image/png":  "image.png",
	"image/webp": "image.webp",
}

// fetchMedia downloads u, which is on media_hosts, following redirects only
// to allowed hosts. A photo must be at most image_max_bytes of JPEG, PNG or
// WebP (judged by its bytes); anything else at most media_max_bytes.
func (t *telegram) fetchMedia(ctx context.Context, u *url.URL, kind string) ([]byte, string, error) {
	if err := validateHTTPURL(u.String(), t.allowPrivate); err != nil {
		return nil, "", fmt.Errorf("media url: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, imageFetchTimeout)
	defer cancel()

	client := *t.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		if !t.settings.mediaHosts.allowsURL(req.URL) {
			return fmt.Errorf("redirect to host %q is not allowed", req.URL.Hostname())
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("media url: %w", err)
	}
	limit := t.settings.mediaMaxBytes
	if kind == "photo" {
		limit = t.settings.imageMaxBytes
		req.Header.Set("Accept", "image/jpeg, image/png, image/webp")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch %s: status %d", kind, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, "", fmt.Errorf("fetch %s: %d bytes is over the %d-byte limit", kind, resp.ContentLength, limit)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", kind, err)
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("fetch %s: over the %d-byte limit", kind, limit)
	}
	if kind != "photo" {
		return data, mediaFilename(u, kind), nil
	}
	sniffed := http.DetectContentType(data)
	filename, ok := photoTypes[sniffed]
	if !ok {
		return nil, "", fmt.Errorf("fetch photo: %s is not a JPEG, PNG or WebP image", sniffed)
	}
	return data, filename, nil
}

// mediaFilename names an uploaded file after its URL's last path element
// (Telegram shows a document's name), else after its kind.
func mediaFilename(u *url.URL, kind string) string {
	name := path.Base(u.Path)
	if name == "." || name == "/" || name == "" {
		return kind
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
}
