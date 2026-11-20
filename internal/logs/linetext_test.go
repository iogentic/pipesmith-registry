package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// forgedText is a complete line of standard error that a value with a line
// break would start, were the break written raw.
const forgedText = "2026-11-19 00:00:00,000 INFO pipesmith_registry.server: forged"

// controls are the characters of decision P7-24, each with the escape that
// LineText writes for it, written by hand.
var controls = []struct{ name, text, escaped string }{
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

// pyLineSplit splits where str.splitlines of Python splits (LF, CR, VT, FF,
// FS, GS, RS, U+0085, U+2028, U+2029; the list of forwarded_test.go of
// internal/server) and at the raw bytes 0x85 and 0x9b.
func pyLineSplit(text string) []string {
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

// rawControl is the first control character of P7-24 in line, or "".
func rawControl(line string) string {
	for i := 0; i < len(line); {
		r, size := utf8.DecodeRuneInString(line[i:])
		if (r == utf8.RuneError && size == 1) || r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return line[i : i+size]
		}
		i += size
	}
	return ""
}

func TestLineText(t *testing.T) {
	for _, c := range controls {
		if got, want := LineText("a"+c.text+forgedText), "a"+c.escaped+forgedText; got != want {
			t.Errorf("%s: %q, want %q", c.name, got, want)
		}
	}
	for _, text := range []string{"", "plain ASCII", `a \ backslash`, `C:\Users\demo-admin\file.txt`, `\x0a stays`, `"quotes" and 'quotes'`,
		"Gr\u00fc\u00dfe", "\u65e5\u672c\u8a9e", "\U0001f680", "\ufffd"} {
		if got := LineText(text); got != text {
			t.Errorf("%q changed to %q", text, got)
		}
	}
	if got, want := LineText("\x80\xc2\xe2\x80"), `\x80\xc2\xe2\x80`; got != want {
		t.Errorf("invalid UTF-8: %q, want %q", got, want)
	}
}

// valid is the text encoding/json gives back for s: a byte that is not
// valid UTF-8 becomes U+FFFD.
func valid(s string) string { return strings.ToValidUTF8(s, "\ufffd") }

// jsonLine checks one line of a JSON file: no control character, valid
// UTF-8, and it parses.
func jsonLine(t *testing.T, name, line string) map[string]any {
	t.Helper()
	if c := rawControl(line); c != "" {
		t.Errorf("%s: raw %q in %q", name, c, line)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("%s: %v: %q", name, err, line)
	}
	return v
}

// No JSON file holds a control character: a server record with one in the
// message, the logger, an attribute value or an attribute name, a request,
// an audit event and a task event with one in a field are one line each by
// the split of Python, and the field holds the original text after decoding.
func TestJSONLinesHoldNoControl(t *testing.T) {
	dir := t.TempDir()
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock})
	h := f.Handler()
	for _, c := range controls {
		hostile := "demo" + c.text + forgedText
		for _, r := range []slog.Record{
			record("message "+hostile, "logger", "pipesmith_registry.server"),
			record("logger", "logger", hostile),
			record("value", "logger", "pipesmith_registry.server", "path", hostile),
			record("name", "logger", "pipesmith_registry.server", hostile, "x"),
		} {
			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatal(err)
			}
		}
		r := demoRequest("/repository/demo-raw/" + hostile)
		r.UserAgent, r.Subject = hostile, hostile
		f.Request(r)
		a := demoAudit("ok")
		a.ObjectName = hostile
		if err := f.Audit(a); err != nil {
			t.Fatal(err)
		}
		e := demoTask("end")
		e.TaskType = hostile
		if err := f.Task(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil || f.Errors() != 0 {
		t.Fatalf("close %v, %d errors", err, f.Errors())
	}
	read := func(name string) []string {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return pyLineSplit(strings.TrimSuffix(string(data), "\n"))
	}
	server, requests, audit, tasks := read("server.log"), read("requests.log"), read("audit.log"), read("tasks.log")
	if len(server) != 4*len(controls) || len(requests) != len(controls) || len(audit) != len(controls) || len(tasks) != len(controls) {
		t.Fatalf("lines: server %d, requests %d, audit %d, tasks %d for %d characters", len(server), len(requests), len(audit), len(tasks),
			len(controls))
	}
	for i, c := range controls {
		hostile := valid("demo" + c.text + forgedText)
		check := func(name string, got any, want string) {
			t.Helper()
			if got != want {
				t.Errorf("%s, %s: %q, want %q", c.name, name, got, want)
			}
		}
		msg := jsonLine(t, "server.log", server[4*i])
		check("msg", msg["msg"], valid("message "+hostile))
		check("logger", jsonLine(t, "server.log", server[4*i+1])["logger"], hostile)
		check("value", jsonLine(t, "server.log", server[4*i+2])["path"], hostile)
		if v := jsonLine(t, "server.log", server[4*i+3]); v[hostile] != "x" {
			t.Errorf("%s, name: %v", c.name, v)
		}
		req := jsonLine(t, "requests.log", requests[i])
		check("user_agent", req["user_agent"], hostile)
		check("subject", req["subject"], hostile)
		check("path", req["path"], valid("/repository/demo-raw/"+hostile))
		check("object_name", jsonLine(t, "audit.log", audit[i])["object_name"], hostile)
		check("task_type", jsonLine(t, "tasks.log", tasks[i])["task_type"], hostile)
	}
}

func record(msg string, args ...any) slog.Record {
	r := slog.NewRecord(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC), slog.LevelInfo, msg, 0)
	r.Add(args...)
	return r
}

// requests-classic.log takes a request with a control character in the user
// agent, the subject and the path as one line, which the parser of
// classic_test.go reads back.
func TestClassicLineOfAControl(t *testing.T) {
	dir := t.TempDir()
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock})
	var sent []Request
	for i, c := range controls {
		hostile := "demo" + c.text + forgedText
		r := demoRequest("/repository/demo-raw/" + hostile)
		r.UserAgent, r.Subject, r.ID = hostile, hostile, fmt.Sprintf("%016x", i)
		f.Request(r)
		sent = append(sent, r)
	}
	if err := f.Close(); err != nil || f.Errors() != 0 {
		t.Fatalf("close %v, %d errors", err, f.Errors())
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests-classic.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := pyLineSplit(strings.TrimSuffix(string(data), "\n"))
	if len(lines) != len(sent) {
		t.Fatalf("%d lines for %d requests", len(lines), len(sent))
	}
	for i, line := range lines {
		rec, err := parseClassic(line)
		if err != nil {
			t.Fatalf("%s: %v: %q", controls[i].name, err, line)
		}
		if rec.userAgent != sent[i].UserAgent || rec.subject != sent[i].Subject || !strings.HasPrefix(rec.target, sent[i].Path) {
			t.Errorf("%s: %+v", controls[i].name, rec)
		}
	}
}

// Lines keeps the line break of its format and escapes every text argument;
// server.log leaves the attribute out and keeps the message as logged.
func TestLinesOfAFormat(t *testing.T) {
	a := Lines("scan of store %s failed\n%s: %s (%d)", "demo"+"\n"+forgedText, "OSError", fmt.Errorf("bad\r%s", forgedText), 7)
	text, ok := LinesText(a)
	if want := "scan of store demo\\x0a" + forgedText + " failed\nOSError: bad\\x0d" + forgedText + " (7)"; !ok || text != want {
		t.Fatalf("%v %q, want %q", ok, text, want)
	}
	if _, ok := LinesText(slog.String(a.Key, text)); ok {
		t.Fatal("a string attribute of the same key counts as Lines")
	}
	dir := t.TempDir()
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock})
	msg := "scan of store demo failed\nOSError: bad"
	if err := f.Handler().Handle(context.Background(), record(msg, "logger", "pipesmith_registry.server", a)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got := lines(t, dir, "server.log")
	if len(got) != 1 {
		t.Fatalf("%q", got)
	}
	keys, values := keysOf(t, got[0])
	if strings.Join(keys, ",") != "time,level,logger,msg" || values["msg"] != msg {
		t.Fatalf("%v %v", keys, values)
	}
}
