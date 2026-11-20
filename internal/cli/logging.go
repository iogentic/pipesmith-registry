package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync"

	"github.com/iogentic/pipesmith-registry/internal/logs"
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
	line := stderrMessage(r)
	if h.basic {
		defer h.tee(r) // after the line and the unlock below
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
			logs.LineText(logger), line)
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

// sinks are the second sinks of the records of serve, by the writer of the
// log handler: the server log of logfiles.go for standard error.
var sinks = struct {
	sync.RWMutex
	m map[io.Writer]slog.Handler
}{m: map[io.Writer]slog.Handler{}}

// keyed is a writer that can be a map key.
func keyed(w io.Writer) bool { return w != nil && reflect.TypeOf(w).Comparable() }

// setSink makes h the second sink of the handlers that write to w; nil
// removes it.
func setSink(w io.Writer, h slog.Handler) {
	if !keyed(w) {
		return
	}
	sinks.Lock()
	defer sinks.Unlock()
	if h == nil {
		delete(sinks.m, w)
	} else {
		sinks.m[w] = h
	}
}

// tee hands a record, with the attributes of the handler, to the second
// sink of its writer.
func (h *logHandler) tee(r slog.Record) {
	if !keyed(h.w) {
		return
	}
	sinks.RLock()
	sink := sinks.m[h.w]
	sinks.RUnlock()
	if sink == nil || !sink.Enabled(context.Background(), r.Level) {
		return
	}
	if len(h.attrs) > 0 {
		sink = sink.WithAttrs(h.attrs)
	}
	_ = sink.Handle(context.Background(), r) // the sink counts a line it cannot write
}
