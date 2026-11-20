package logs

import (
	"context"
	"log/slog"
)

// handler is the slog.Handler of server.log. The logger field is the last
// logger attribute of the handler or the record; the other attributes
// follow msg in their order, those of the handler first. WithGroup keeps
// the handler, as the handler of standard error does: this server logs no
// groups.
type handler struct {
	files  *Files
	levels *Levels // the level of each logger (levels.go); nil drops no record
	logger string
	attrs  []byte // the encoded attributes of WithAttrs
}

func (h *handler) Enabled(context.Context, slog.Level) bool { return true }

// Handle writes a line from WARN upward before it returns; a line below
// waits for the next flush.
func (h *handler) Handle(_ context.Context, r slog.Record) error {
	if h.files.idle() {
		return nil
	}
	logger, attrs := h.logger, h.attrs[:len(h.attrs):len(h.attrs)] // an append copies, so records do not share bytes
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			logger = a.Value.Resolve().String()
		} else if _, isLines := LinesText(a); !isLines {
			attrs = appendAttr(attrs, a, true)
		}
		return true
	})
	if logger == "" {
		logger = loggerDefault
	}
	if !h.levels.Enabled(logger, r.Level) {
		return nil
	}
	t := r.Time
	if t.IsZero() {
		t = h.files.now()
	}
	line := serverLine(t, r.Level, logger, r.Message, attrs)
	if r.Level >= slog.LevelWarn {
		return h.files.server.flush(line)
	}
	h.files.server.add(line)
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &handler{files: h.files, levels: h.levels, logger: h.logger, attrs: append([]byte(nil), h.attrs...)}
	for _, a := range attrs {
		if a.Key == "logger" {
			next.logger = a.Value.Resolve().String()
			continue
		}
		next.attrs = appendAttr(next.attrs, a, true)
	}
	return next
}

func (h *handler) WithGroup(string) slog.Handler { return h }
