package providers

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeTelegram is a stand-in Bot API. It records every call and answers each
// method from answers (default: 200 {"ok":true,"result":true}).
type fakeTelegram struct {
	srv *httptest.Server

	mu      sync.Mutex
	calls   []tgCall
	answers map[string]func(c tgCall) (int, string)
}

// tgCall is one recorded Bot API call.
type tgCall struct {
	Method   string
	Token    string
	JSON     map[string]any    // a JSON body
	Form     map[string]string // a multipart body's fields
	File     []byte            // a multipart body's photo
	FileName string
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{answers: map[string]func(tgCall) (int, string){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path: /bot<token>/<method>
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/bot"), "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		c := tgCall{Token: parts[0], Method: parts[1]}
		mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType == "multipart/form-data" {
			c.Form = map[string]string{}
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				b, _ := io.ReadAll(part)
				if part.FormName() == "photo" {
					c.File, c.FileName = b, part.FileName()
				} else {
					c.Form[part.FormName()] = string(b)
				}
			}
		} else {
			_ = json.NewDecoder(r.Body).Decode(&c.JSON)
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		answer := f.answers[c.Method]
		f.mu.Unlock()
		status, body := http.StatusOK, `{"ok":true,"result":true}`
		if answer != nil {
			status, body = answer(c)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// answer sets how a method is answered.
func (f *fakeTelegram) answer(method string, fn func(c tgCall) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = fn
}

// callsTo returns the recorded calls of one method, in order.
func (f *fakeTelegram) callsTo(method string) []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []tgCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// methods returns the methods called, in order.
func (f *fakeTelegram) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

// keyboardOf decodes a call's reply_markup (JSON body or multipart field).
func keyboardOf(t *testing.T, c tgCall) *tgKeyboard {
	t.Helper()
	var raw []byte
	switch {
	case c.Form != nil:
		if c.Form["reply_markup"] == "" {
			return nil
		}
		raw = []byte(c.Form["reply_markup"])
	case c.JSON["reply_markup"] != nil:
		raw, _ = json.Marshal(c.JSON["reply_markup"])
	default:
		return nil
	}
	var kb tgKeyboard
	if err := json.Unmarshal(raw, &kb); err != nil {
		t.Fatalf("bad reply_markup %s: %v", raw, err)
	}
	return &kb
}

// testPNG returns a small valid PNG.
func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 6))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// imageServer serves body with contentType at every path and counts requests.
func imageServer(t *testing.T, contentType string, body []byte) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}
