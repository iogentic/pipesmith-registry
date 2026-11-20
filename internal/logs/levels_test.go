package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLevelsListEveryLoggerAtInfo(t *testing.T) {
	l := NewLevels(nil)
	list := l.List()
	if len(list) != len(Loggers) {
		t.Fatalf("%d loggers listed, want %d", len(list), len(Loggers))
	}
	for i, row := range list {
		if row.Name != Loggers[i] || row.Level != "INFO" || row.Default != "INFO" {
			t.Errorf("row %d: %+v", i, row)
		}
	}
	if !slices.IsSorted(Loggers) {
		t.Errorf("Loggers is not sorted: %v", Loggers)
	}
}

func TestLevelsSetRemoveResetSave(t *testing.T) {
	var saved []map[string]string
	l := NewLevels(func(m map[string]string) error {
		saved = append(saved, m)
		return nil
	})
	if err := l.Set("pipesmith_registry.proxy", "WARN"); err != nil {
		t.Fatal(err)
	}
	if err := l.Set("pipesmith_registry.catalog", "ERROR"); err != nil {
		t.Fatal(err)
	}
	if l.Enabled("pipesmith_registry.proxy", slog.LevelInfo) || !l.Enabled("pipesmith_registry.proxy", slog.LevelWarn) {
		t.Error("the proxy at WARN lets INFO through or holds WARN back")
	}
	if !l.Enabled("pipesmith_registry.server", slog.LevelInfo) {
		t.Error("a logger without a level holds INFO back")
	}
	if err := l.Remove("pipesmith_registry.proxy"); err != nil {
		t.Fatal(err)
	}
	if !l.Enabled("pipesmith_registry.proxy", slog.LevelInfo) {
		t.Error("the removed level still holds INFO back")
	}
	if err := l.Reset(); err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{
		{"pipesmith_registry.proxy": "WARN"},
		{"pipesmith_registry.proxy": "WARN", "pipesmith_registry.catalog": "ERROR"},
		{"pipesmith_registry.catalog": "ERROR"},
		{},
	}
	if len(saved) != len(want) {
		t.Fatalf("saved %v, want %v", saved, want)
	}
	for i := range want {
		if !mapsEqual(saved[i], want[i]) {
			t.Errorf("save %d: %v, want %v", i, saved[i], want[i])
		}
	}
	// INFO is the default and is not stored
	if err := l.Set("pipesmith_registry.catalog", "INFO"); err != nil || len(saved[len(saved)-1]) != 0 {
		t.Errorf("INFO stored: %v %v", err, saved[len(saved)-1])
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestLevelsRefuseAnUnknownNameOrLevel(t *testing.T) {
	calls := 0
	l := NewLevels(func(map[string]string) error { calls++; return nil })
	for _, name := range []string{"pipesmith_registry.demo", "", "root", "pipesmith_registry.requests", "pipesmith_registry.audit"} {
		if err := l.Set(name, "WARN"); !errors.Is(err, ErrUnknownLogger) {
			t.Errorf("Set %q: %v", name, err)
		}
		if err := l.Remove(name); !errors.Is(err, ErrUnknownLogger) {
			t.Errorf("Remove %q: %v", name, err)
		}
	}
	for _, level := range []string{"", "warn", "TRACE", "WARNING", "FATAL"} {
		if err := l.Set("pipesmith_registry.server", level); !errors.Is(err, ErrLevel) {
			t.Errorf("level %q: %v", level, err)
		}
	}
	if calls != 0 {
		t.Errorf("%d saves of a refused change", calls)
	}
	if len(l.List()) != len(Loggers) {
		t.Error("a refused name was added")
	}
}

func TestLevelsKeepTheOldLevelWhenTheSaveFails(t *testing.T) {
	l := NewLevels(func(map[string]string) error { return errors.New("disk full") })
	if err := l.Set("pipesmith_registry.server", "ERROR"); err == nil {
		t.Fatal("a failed save gives no error")
	}
	if !l.Enabled("pipesmith_registry.server", slog.LevelInfo) {
		t.Error("the level changed although the save failed")
	}
}

func TestLevelsLoadTheSetting(t *testing.T) {
	l := NewLevels(nil)
	warnings := l.Load([]byte(`{"pipesmith_registry.proxy": "ERROR", "pipesmith_registry.demo": "WARN", "pipesmith_registry.server": "LOUD"}`))
	if len(warnings) != 2 {
		t.Errorf("warnings %v", warnings)
	}
	if l.Enabled("pipesmith_registry.proxy", slog.LevelWarn) || !l.Enabled("pipesmith_registry.server", slog.LevelInfo) {
		t.Error("the setting was not applied as read")
	}
	if w := NewLevels(nil).Load([]byte(`[1]`)); len(w) != 1 {
		t.Errorf("a setting that is no object: %v", w)
	}
}

func TestANilLevelsLetsEverythingThrough(t *testing.T) {
	var l *Levels
	if !l.Enabled("pipesmith_registry.server", slog.LevelDebug) {
		t.Error("nil holds a line back")
	}
}

// TestServerLogKeepsTheLevels: the handler of server.log, and with it the
// lines shipped to the log targets, drops a record below the level of its
// logger, by the logger of the handler or of the record.
func TestServerLogKeepsTheLevels(t *testing.T) {
	dir := t.TempDir()
	var shipped []Line
	f := Open(Options{Dir: dir, Ship: func(l Line) { shipped = append(shipped, l) }})
	l := NewLevels(nil)
	if err := l.Set("pipesmith_registry.proxy", "WARN"); err != nil {
		t.Fatal(err)
	}
	h := f.HandlerAt(l)
	proxy := h.WithAttrs([]slog.Attr{slog.String("logger", "pipesmith_registry.proxy")})
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	records := []struct {
		h      slog.Handler
		level  slog.Level
		msg    string
		logger string
	}{
		{proxy, slog.LevelInfo, "proxy info dropped", ""},
		{proxy, slog.LevelWarn, "proxy warn kept", ""},
		{h, slog.LevelInfo, "record proxy info dropped", "pipesmith_registry.proxy"},
		{h, slog.LevelInfo, "server info kept", "pipesmith_registry.server"},
		{proxy, slog.LevelInfo, "record names the server, kept", "pipesmith_registry.server"},
	}
	for _, r := range records {
		rec := slog.NewRecord(at, r.level, r.msg, 0)
		if r.logger != "" {
			rec.AddAttrs(slog.String("logger", r.logger))
		}
		if err := r.h.Handle(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		got = append(got, m["msg"].(string))
	}
	want := []string{"proxy warn kept", "server info kept", "record names the server, kept"}
	if !slices.Equal(got, want) {
		t.Errorf("server.log holds %q, want %q", got, want)
	}
	if len(shipped) != len(want) {
		t.Errorf("%d lines shipped, want %d", len(shipped), len(want))
	}
}

// loggerLiteral is a logger name written in the code.
var loggerLiteral = regexp.MustCompile(`"(pipesmith_registry(\.[a-z_]+)?)"`)

// notLevelled are the names in the code that are no logger of server.log:
// the lines of requests.log and audit.log, and the attribute key of Lines.
var notLevelled = []string{"pipesmith_registry.audit", "pipesmith_registry.lines", "pipesmith_registry.requests"}

// TestLoggersHoldEveryNameOfTheCode walks the Go files outside tests: every
// logger name the code writes is in Loggers, and every name of Loggers is
// written by the code (L10: no empty row).
func TestLoggersHoldEveryNameOfTheCode(t *testing.T) {
	root := filepath.Join("..", "..")
	found := map[string]bool{"pipesmith_registry": true} // loggerDefault, a record without a logger
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range loggerLiteral.FindAllStringSubmatch(string(data), -1) {
				found[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for name := range found {
		if !slices.Contains(Loggers, name) && !slices.Contains(notLevelled, name) {
			t.Errorf("the code writes the logger %s, which Loggers does not hold", name)
		}
	}
	for _, name := range Loggers {
		if !found[name] {
			t.Errorf("Loggers holds %s, which no code writes", name)
		}
	}
	if len(found) < 10 {
		t.Fatalf("only %d names found; the walk missed the code", len(found))
	}
}
