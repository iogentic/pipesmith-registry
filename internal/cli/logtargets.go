package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/logship"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/version"
)

// logTargets starts the senders of the log targets of serve: the rows of the
// setting log_targets of state.sqlite and their secret files, read again
// every 5 s. Its lines reach standard error and server.log, never a target.
func (c *command) logTargets(dataDir string) *logship.Manager {
	return logship.New(logship.Options{
		Rows:    func() ([]byte, error) { return targetSetting(dataDir) },
		Secret:  targetSecretFile(dataDir),
		Logger:  slog.New(&targetLog{err: c.err}),
		Version: version.Version,
	})
}

// targetSetting is the value of the setting log_targets; none without
// state.sqlite or without the row.
func targetSetting(dataDir string) ([]byte, error) {
	st, err := state.Open(dataDir)
	if errors.Is(err, state.ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }() // read only; nothing to keep
	value, _, err := st.Setting(logship.SettingKey)
	return value, err
}

// targetLog is the handler of the lines of the log targets. Standard error
// shows the message with its attributes, so a warning names its target and
// file there too; server.log gets the record as it is.
type targetLog struct {
	err   io.Writer
	attrs []slog.Attr
}

// plainWriter writes to standard error without the second sink of
// logging.go: a struct with a func field is no map key.
type plainWriter struct{ write func([]byte) (int, error) }

func (w plainWriter) Write(p []byte) (int, error) { return w.write(p) }

func (h *targetLog) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelInfo }

func (h *targetLog) Handle(ctx context.Context, r slog.Record) error {
	var text strings.Builder
	text.WriteString(r.Message)
	var logger []slog.Attr
	add := func(a slog.Attr) bool {
		if a.Key == "logger" {
			logger = []slog.Attr{a}
			return true
		}
		value := a.Value.Resolve().String()
		if value == "" || strings.ContainsAny(value, " \"=") {
			value = strconv.Quote(value)
		}
		text.WriteString(" " + a.Key + "=" + value)
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	plain := slog.NewRecord(r.Time, r.Level, text.String(), r.PC)
	plain.AddAttrs(logger...)
	err := newLogHandler(plainWriter{h.err.Write}, true).Handle(ctx, plain)
	sink := newLogHandler(h.err, true)
	sink.attrs = h.attrs
	sink.tee(r)
	return err
}

func (h *targetLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &targetLog{err: h.err, attrs: append(slices.Clone(h.attrs), attrs...)}
}

func (h *targetLog) WithGroup(string) slog.Handler { return h }
