package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// forgedText is a complete line of standard error that a value with a line
// break would start, were the break written raw.
const forgedText = "2026-11-19 00:00:00,000 INFO pipesmith_registry.server: forged"

// controlChars are the characters of decision P7-24 with the escape that
// standard error writes for each, written by hand.
var controlChars = []struct{ name, text, escaped string }{
	{"line feed", "\n", `\x0a`},
	{"carriage return", "\r", `\x0d`},
	{"tab", "\t", `\x09`},
	{"vertical tab", "\v", `\x0b`},
	{"form feed", "\f", `\x0c`},
	{"escape", "\x1b", `\x1b`},
	{"nul", "\x00", `\x00`},
	{"0x1c", "\x1c", `\x1c`},
	{"0x1d", "\x1d", `\x1d`},
	{"0x1e", "\x1e", `\x1e`},
	{"0x7f", "\x7f", `\x7f`},
	{"U+0085", "\u0085", `\u0085`},
	{"U+009B", "\u009b", `\u009b`},
	{"U+2028", "\u2028", `\u2028`},
	{"U+2029", "\u2029", `\u2029`},
	{"byte 0x85", "\x85", `\x85`},
	{"byte 0x9b", "\x9b", `\x9b`},
}

// textLines splits where str.splitlines of Python splits (LF, CR, VT, FF,
// FS, GS, RS, U+0085, U+2028, U+2029; the list of forwarded_test.go of
// internal/server) and at the raw bytes 0x85 and 0x9b.
func textLines(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		cut := false
		switch {
		case r == utf8.RuneError && size == 1:
			cut = text[i] == 0x85 || text[i] == 0x9b
		case r == '\n', r == '\r', r == '\v', r == '\f', r == 0x1c, r == 0x1d, r == 0x1e, r == 0x85, r == 0x2028, r == 0x2029:
			cut = true
		}
		if cut {
			out = append(out, text[start:i])
			start = i + size
		}
		i += size
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

var controlTime = time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)

func controlRecord(level slog.Level, msg string, args ...any) slog.Record {
	r := slog.NewRecord(controlTime, level, msg, 0)
	r.Add(args...)
	return r
}

// oneLine handles r and checks that standard error got exactly one line by
// the split of Python, that no line starts with the forged text and that
// the line holds want.
func oneLine(t *testing.T, name string, h slog.Handler, r slog.Record, want string) {
	t.Helper()
	var buf bytes.Buffer
	switch h := h.(type) {
	case *logHandler:
		h.w = &buf
	case fieldHandler:
		h.Handler.(*logHandler).w = &buf
	}
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("%s: no line break at the end: %q", name, out)
	}
	got := textLines(strings.TrimSuffix(out, "\n"))
	if len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("%s: %d lines %q, want one with %q", name, len(got), got, want)
	}
	for _, line := range got {
		if strings.HasPrefix(line, forgedText) {
			t.Errorf("%s: a line starts with the forged text: %q", name, out)
		}
	}
}

// Standard error writes one line per record with a control character in the
// message, the logger name, an attribute value or an attribute name: the
// handler of serve, the last resort handler and the security logger
// through fieldHandler.
func TestStderrEscapesControls(t *testing.T) {
	for _, c := range controlChars {
		hostile := "demo" + c.text + forgedText
		escaped := "demo" + c.escaped + forgedText
		oneLine(t, c.name+", serve, message", newLogHandler(nil, true),
			controlRecord(slog.LevelInfo, "message "+hostile, "logger", "pipesmith_registry.server"), "message "+escaped)
		oneLine(t, c.name+", serve, logger", newLogHandler(nil, true),
			controlRecord(slog.LevelInfo, "message", "logger", hostile), " "+escaped+": message")
		oneLine(t, c.name+", last resort, message", newLogHandler(nil, false),
			controlRecord(slog.LevelWarn, "message "+hostile), "message "+escaped)
		oneLine(t, c.name+", security, value", fieldHandler{newLogHandler(nil, true)},
			controlRecord(slog.LevelWarn, "message", "logger", securityLogger, "user", hostile), "message: user="+escaped)
		oneLine(t, c.name+", security, name", fieldHandler{newLogHandler(nil, true)},
			controlRecord(slog.LevelWarn, "message", "logger", securityLogger, hostile, "x"), "message: "+escaped+"=x")
	}
}

// The second sink of standard error, server.log, writes one JSON line per
// record with the original text after decoding.
func TestServerLogOfTheStderrHandlerHoldsNoControl(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	files := logs.Open(logs.Options{Dir: dir, Settings: logs.DefaultSettings()})
	setSink(&buf, files.Handler())
	t.Cleanup(func() { setSink(&buf, nil) })
	h := newLogHandler(&buf, true)
	for _, c := range controlChars {
		hostile := "demo" + c.text + forgedText
		for _, r := range []slog.Record{
			controlRecord(slog.LevelWarn, "message "+hostile, "logger", "pipesmith_registry.server"),
			controlRecord(slog.LevelWarn, "message", "logger", hostile),
			controlRecord(slog.LevelWarn, "message", "logger", "pipesmith_registry.server", "path", hostile),
			controlRecord(slog.LevelWarn, "message", "logger", "pipesmith_registry.server", hostile, "x"),
		} {
			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := textLines(strings.TrimSuffix(string(data), "\n"))
	if len(got) != 4*len(controlChars) {
		t.Fatalf("%d lines for %d records", len(got), 4*len(controlChars))
	}
	for i, c := range controlChars {
		hostile := strings.ToValidUTF8("demo"+c.text+forgedText, "\ufffd")
		var v [4]map[string]any
		for j := range v {
			if err := json.Unmarshal([]byte(got[4*i+j]), &v[j]); err != nil {
				t.Fatalf("%s: %v: %q", c.name, err, got[4*i+j])
			}
		}
		if v[0]["msg"] != "message "+hostile || v[1]["logger"] != hostile || v[2]["path"] != hostile || v[3][hostile] != "x" {
			t.Errorf("%s: %v", c.name, v)
		}
	}
}

// Decision L2: a line without a control character is byte equal to the line
// that logging.go writes without the escape, written here by hand from its
// format "%(asctime)s %(levelname)s %(name)s: %(message)s" and, for the last
// resort handler, the message alone.
func TestStderrOrdinaryLinesUnchanged(t *testing.T) {
	stamp := controlTime.Format("2006-01-02 15:04:05") + ",000"
	for _, msg := range []string{
		"listening on 127.0.0.1 port 14193",
		`a backslash \ and \x0a as text`,
		`C:\Users\demo-admin\Downloads\demo.jar`,
		`"double" and 'single' quotes`,
		"Gr\u00fc\u00dfe aus M\u00fcnchen",
		"\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8",
		"a rocket \U0001f680 in the text",
		"",
	} {
		var serve, last bytes.Buffer
		if err := newLogHandler(&serve, true).Handle(context.Background(),
			controlRecord(slog.LevelInfo, msg, "logger", "pipesmith_registry.server")); err != nil {
			t.Fatal(err)
		}
		if got, want := serve.String(), stamp+" INFO pipesmith_registry.server: "+msg+"\n"; got != want {
			t.Errorf("serve: %q, want %q", got, want)
		}
		if err := newLogHandler(&last, false).Handle(context.Background(), controlRecord(slog.LevelWarn, msg)); err != nil {
			t.Fatal(err)
		}
		if got, want := last.String(), msg+"\n"; got != want {
			t.Errorf("last resort: %q, want %q", got, want)
		}
	}
}

// A record with logs.Lines writes the line break of its own format, as the
// old product writes a traceback head, and nothing else: its values are
// escaped. server.log holds the message as logged.
func TestStderrLinesOfAFormat(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	files := logs.Open(logs.Options{Dir: dir, Settings: logs.DefaultSettings()})
	setSink(&buf, files.Handler())
	t.Cleanup(func() { setSink(&buf, nil) })
	store := "demo\n" + forgedText
	msg := "scan of store " + store + " failed\nOSError: [Errno 5] Input/output error"
	r := controlRecord(slog.LevelError, msg, "logger", "pipesmith_registry.server",
		logs.Lines("scan of store %s failed\n%s: %s", store, "OSError", "[Errno 5] Input/output error"))
	if err := newLogHandler(&buf, true).Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	stamp := controlTime.Format("2006-01-02 15:04:05") + ",000"
	want := stamp + " ERROR pipesmith_registry.server: scan of store demo\\x0a" + forgedText + " failed\nOSError: [Errno 5] Input/output error\n"
	if got := buf.String(); got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(bytes.TrimSuffix(data, []byte("\n")), &v); err != nil || v["msg"] != msg || len(v) != 4 {
		t.Fatalf("%v %v: %q", err, v, data)
	}
}
