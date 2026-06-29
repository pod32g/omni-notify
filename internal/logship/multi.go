package logship

import (
	"context"
	"errors"
	"log/slog"
)

// multiHandler fans each record out to several slog.Handlers (a "tee"), so
// omni-notify can keep logging to stdout while also forwarding to omni-logging.
type multiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler returns a handler that dispatches to each non-nil handler.
func NewMultiHandler(handlers ...slog.Handler) slog.Handler {
	hs := make([]slog.Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			hs = append(hs, h)
		}
	}
	return &multiHandler{handlers: hs}
}

func (m *multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		// Clone so a handler that mutates the record can't affect the others.
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: hs}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return m
	}
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: hs}
}
