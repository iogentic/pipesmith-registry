package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// forgedText is a complete line of standard error that a value with a line
// break would start, were the break written raw.
const forgedText = "2026-11-19 00:00:00,000 INFO pipesmith_registry.server: forged"

// fileLog is the default logger of a test as serve sets it up for the log
// files: every record goes to server.log of a logs.Files in a test
// directory and is kept.
type fileLog struct {
	files   *logs.Files
	dir     string
	mu      sync.Mutex
	records []slog.Record
}

func (f *fileLog) Enabled(context.Context, slog.Level) bool { return true }

func (f *fileLog) Handle(ctx context.Context, r slog.Record) error {
	f.mu.Lock()
	f.records = append(f.records, r.Clone())
	f.mu.Unlock()
	return f.files.Handler().Handle(ctx, r)
}

func (f *fileLog) WithAttrs([]slog.Attr) slog.Handler { return f }
func (f *fileLog) WithGroup(string) slog.Handler      { return f }

func captureFiles(t *testing.T) *fileLog {
	t.Helper()
	f := &fileLog{dir: t.TempDir()}
	f.files = logs.Open(logs.Options{Dir: f.dir, Settings: logs.DefaultSettings()})
	previous := slog.Default()
	slog.SetDefault(slog.New(f))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return f
}

// check closes the files and checks the records whose message starts with
// prefix: exactly one, its message holds the hostile value as it came, its
// text for standard error (logs.LineText, which the handler of
// internal/cli applies; TestStderrEscapesControls there) is one line by the
// split of Python, and server.log holds one line for it with the message
// after decoding.
func (f *fileLog) check(t *testing.T, prefix, hostile string) {
	t.Helper()
	if err := f.files.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	var found []string
	for _, r := range f.records {
		if strings.HasPrefix(r.Message, prefix) {
			found = append(found, r.Message)
		}
	}
	f.mu.Unlock()
	if len(found) != 1 || !strings.Contains(found[0], hostile) {
		t.Fatalf("records %q, want one with %q", found, hostile)
	}
	if lines := pyLines(logs.LineText(found[0])); len(lines) != 1 {
		t.Errorf("standard error: %d lines %q", len(lines), lines)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(prefix)
	var mine []string
	for _, line := range pyLines(string(data)) {
		if strings.Contains(line, `"msg":`+string(quoted[:len(quoted)-1])) {
			mine = append(mine, line)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("server.log: %d lines for the record: %q", len(mine), data)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(mine[0]), &v); err != nil || v["msg"] != found[0] {
		t.Fatalf("server.log %v %v, want msg %q", err, v, found[0])
	}
	for _, line := range pyLines(string(data)) {
		if strings.HasPrefix(line, forgedText) {
			t.Errorf("server.log: a line starts with the forged text: %q", line)
		}
	}
}

// refusingCatalog fails every write and delete with an error that holds a
// line feed and the path of the request.
type refusingCatalog struct{}

func (refusingCatalog) Put(asset index.Asset, _ stores.Digests, _, _ string) error {
	return fmt.Errorf("database is locked for %s\n%s", asset.Path, forgedText)
}

func (refusingCatalog) Delete(_, path string) error {
	return fmt.Errorf("database is locked for %s\n%s", path, forgedText)
}

// The catalog lines of ingest.go: a raw PUT and DELETE of a path with U+2028
// and a forged line, and a catalog error with a line feed, are one record
// each and one line in server.log.
func TestCatalogLinesOfAHostilePath(t *testing.T) {
	for _, site := range []struct{ method, prefix string }{
		{"PUT", "the catalog row of demo-raw/docs/a "},
		{"DELETE", "the catalog row of demo-raw/docs/a "},
	} {
		t.Run(site.method, func(t *testing.T) {
			w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
			w.srv.writes.catalog = refusingCatalog{}
			target := "/repository/demo-raw/docs/a" + url.PathEscape(" "+forgedText) + ".txt"
			if site.method == "DELETE" {
				if r := do(t, w.addr(), "PUT", target, []byte("x"), "basic", asAdmin); r.status != 201 {
					t.Fatalf("PUT: %d %q", r.status, r.body)
				}
			}
			log := captureFiles(t)
			want := http.StatusCreated
			if site.method == "DELETE" {
				want = http.StatusNoContent
			}
			if r := do(t, w.addr(), site.method, target, []byte("x"), "basic", asAdmin); r.status != want {
				t.Fatalf("%s: %d %q", site.method, r.status, r.body)
			}
			log.check(t, site.prefix, " "+forgedText+".txt\n"+forgedText)
		})
	}
}

// failingSecurity resolves no caller of a request that sends X-Demo-Fail:
// its error names the query value user, as a directory error may name the
// user it looked up.
type failingSecurity struct{ *fakeSecurity }

func (f failingSecurity) Subject(r *http.Request) (*model.Subject, error) {
	if r.Header.Get("X-Demo-Fail") != "" {
		return nil, errors.New("the directory has no answer for " + r.URL.Query().Get("user"))
	}
	return f.fakeSecurity.Subject(r)
}

// fail() of server.go: a request whose query value holds a line feed and a
// forged line, and an error that names that value, give one record and one
// line in server.log.
func TestFailOfAHostilePath(t *testing.T) {
	base := t.TempDir()
	cfg := prepare(t, base, []config.StoreConfig{blobStore(t, base, "s1", jarSpec("x"))},
		appOptions{defs: []*pyjson.Object{def("demo-releases", "maven2", "hosted")}})
	a := startApp(t, base, cfg, appOptions{security: failingSecurity{newFakeSecurity()}})
	log := captureFiles(t)
	r := a.get(jarURL+"?user=demo%0A"+url.QueryEscape(forgedText), "X-Demo-Fail", "1")
	if r.status != 500 || string(r.body) != "Internal Server Error" {
		t.Fatalf("%s", r)
	}
	log.check(t, "Exception in ASGI application: Exception: the directory has no answer for demo\n",
		"\n"+forgedText)
}

// The scan failure keeps the line break of its format on standard error and
// escapes the store name, which config.json holds (logLines of scanjob.go).
func TestScanFailureLinesEscapeTheValues(t *testing.T) {
	log := captureFiles(t)
	logLines(slog.LevelError, serverLogger, "scan of store %s failed\n%s: %s", "demo\n"+forgedText, "OSError", "text")
	if err := log.files.Close(); err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.records) != 1 {
		t.Fatalf("%d records", len(log.records))
	}
	var text string
	log.records[0].Attrs(func(a slog.Attr) bool {
		if s, ok := logs.LinesText(a); ok {
			text = s
		}
		return true
	})
	if want := "scan of store demo\\x0a" + forgedText + " failed\nOSError: text"; text != want {
		t.Fatalf("%q, want %q", text, want)
	}
	if msg := log.records[0].Message; msg != "scan of store demo\n"+forgedText+" failed\nOSError: text" {
		t.Fatalf("message %q", msg)
	}
}
