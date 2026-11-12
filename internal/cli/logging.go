package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// logHandler writes log records as the logging module of Python does. With
// basic it is the setup of `serve`, logging.basicConfig with
// "%(asctime)s %(levelname)s %(name)s: %(message)s" from INFO up; without it
// the last resort handler of Python, the message alone from WARNING up.
type logHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	min   slog.Level
	basic bool
	attrs []slog.Attr
}

func newLogHandler(w io.Writer, basic bool) *logHandler {
	min := slog.LevelWarn
	if basic {
		min = slog.LevelInfo
	}
	return &logHandler{mu: &sync.Mutex{}, w: w, min: min, basic: basic}
}

func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARNING"
	case level >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}

func (h *logHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.min }

func (h *logHandler) Handle(_ context.Context, r slog.Record) error {
	line := r.Message
	if h.basic {
		logger := "root"
		find := func(a slog.Attr) bool {
			if a.Key == "logger" {
				logger = a.Value.String()
			}
			return true
		}
		for _, a := range h.attrs {
			find(a)
		}
		r.Attrs(find)
		t := r.Time.Local()
		line = fmt.Sprintf("%s,%03d %s %s: %s", t.Format("2006-01-02 15:04:05"), t.Nanosecond()/1_000_000, levelName(r.Level),
			logger, r.Message)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintln(h.w, line)
	return err
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &next
}

func (h *logHandler) WithGroup(string) slog.Handler { return h }
