package logs

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gzip -t checks the integrity of a compressed file; the exit status is 0 on
// success, 1 on a failure and 2 on a warning. Both from the manual of GNU
// gzip, https://www.gnu.org/software/gzip/manual/gzip.html, section Invoking
// gzip, read on 2026-11-19 with curl.

// fakeClock is the clock of a test, moved by the test; safe for several
// goroutines.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// small are the settings of the rotation tests: a file holds at most 1 KiB.
func small() Settings {
	s := DefaultSettings()
	s.MaxBytes = 1 << 10
	return s
}

// rotatedOf lists the rotated files of kind in dir by day and then by
// number; a rotated file that is not compressed fails the test.
func rotatedOf(t *testing.T, dir, kind string) []string {
	t.Helper()
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(kind) + `-(\d{4}-\d\d-\d\d)\.([1-9][0-9]*)\.log(\.gz)?$`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	type rotated struct {
		day  string
		n    int
		name string
	}
	var list []rotated
	for _, e := range entries {
		m := pattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if m[3] == "" {
			t.Fatalf("%s is not compressed", e.Name())
		}
		n, _ := strconv.Atoi(m[2])
		list = append(list, rotated{m[1], n, e.Name()})
	}
	slices.SortFunc(list, func(a, b rotated) int {
		if c := strings.Compare(a.day, b.day); c != 0 {
			return c
		}
		return a.n - b.n
	})
	var names []string
	for _, r := range list {
		names = append(names, r.name)
	}
	return names
}

// gzipTest runs the gzip program of the system with -t on the files; a
// missing program fails the test.
func gzipTest(t *testing.T, paths ...string) {
	t.Helper()
	if len(paths) == 0 {
		return
	}
	program, err := exec.LookPath("gzip")
	if err != nil {
		t.Fatalf("no gzip program: %v", err)
	}
	if out, err := exec.Command(program, append([]string{"-t"}, paths...)...).CombinedOutput(); err != nil {
		t.Fatalf("gzip -t: %v %s", err, out)
	}
}

// unzipped reads a compressed file whole with compress/gzip.
func unzipped(t *testing.T, path string) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	zr, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return data
}

// writeGzip writes data compressed to path.
func writeGzip(t *testing.T, path string, data []byte) {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// filesOf checks every rotated file of kind with gzip -t and returns the
// lines of each, the active file last.
func filesOf(t *testing.T, dir, kind string) [][]string {
	t.Helper()
	names := rotatedOf(t, dir, kind)
	var paths []string
	for _, name := range names {
		paths = append(paths, filepath.Join(dir, name))
	}
	gzipTest(t, paths...)
	var files [][]string
	for _, path := range paths {
		files = append(files, splitLines(unzipped(t, path)))
	}
	return append(files, lines(t, dir, kind+".log"))
}

// numberOf reads the attribute n of a line of server.log.
func numberOf(t *testing.T, line string) int {
	t.Helper()
	var v struct {
		N *int `json:"n"`
	}
	if err := json.Unmarshal([]byte(line), &v); err != nil || v.N == nil {
		t.Fatalf("no number in %q: %v", line, err)
	}
	return *v.N
}

// A file rotates before a write would take it past MaxBytes: a rotated file
// holds at most 1 KiB, or one line that alone is larger, and the first line
// of the next file would not have fit. The rotated files are
// server-<day>.<n>.log.gz with n from 1, and their lines and those of the
// active file are the lines written, in their order.
func TestRotationAtTheSize(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: small(), Now: clk.now})
	log := slog.New(f.Handler())
	const sent = 40
	for i := range sent {
		pad := strings.Repeat("x", i*7)
		if i == 20 {
			pad = strings.Repeat("y", 1500) // a line larger than the limit
		}
		log.Warn("line", "n", i, "pad", pad)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	names := rotatedOf(t, dir, "server")
	if len(names) < 10 {
		t.Fatalf("%d rotated files: %v", len(names), names)
	}
	for i, name := range names {
		if want := fmt.Sprintf("server-2026-11-19.%d.log.gz", i+1); name != want {
			t.Errorf("rotated file %d is %s, want %s", i+1, name, want)
		}
	}
	files := filesOf(t, dir, "server")
	size := func(lines []string) int {
		n := 0
		for _, l := range lines {
			n += len(l) + 1
		}
		return n
	}
	var all []string
	for i, lines := range files {
		all = append(all, lines...)
		if i == len(files)-1 {
			break
		}
		if s := size(lines); s > 1<<10 && len(lines) != 1 {
			t.Errorf("rotated file %d holds %d bytes in %d lines", i+1, s, len(lines))
		}
		if next := files[i+1]; len(next) > 0 && size(lines)+len(next[0])+1 <= 1<<10 {
			t.Errorf("rotated file %d of %d bytes rotated before a line of %d bytes", i+1, size(lines), len(next[0])+1)
		}
	}
	if len(all) != sent {
		t.Fatalf("%d lines, want %d", len(all), sent)
	}
	for i, line := range all {
		if n := numberOf(t, line); n != i {
			t.Fatalf("line %d holds n %d", i, n)
		}
	}
	if f.Errors() != 0 {
		t.Fatalf("errors %d", f.Errors())
	}
}

// At the day change in UTC the file rotates under the day of its lines, seen
// by Tick and by the next write. Lines that wait in memory at the day change
// end the file of their day; a file that holds no line does not rotate.
func TestRotationAtTheDayChange(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock(time.Date(2026, 11, 19, 23, 59, 58, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clk.now})
	log := slog.New(f.Handler())
	log.Warn("day one written")
	log.Info("day one waiting")
	f.Request(demoRequest("/day-one"))
	clk.set(time.Date(2026, 11, 20, 0, 0, 1, 0, time.UTC))
	f.Tick()
	for _, name := range []string{"server.log", "requests.log", "requests-classic.log"} {
		if got := lines(t, dir, name); got != nil {
			t.Fatalf("%s after the Tick of the new day: %q", name, got)
		}
	}
	log.Info("day two waiting")
	clk.set(time.Date(2026, 11, 21, 0, 0, 0, 500_000_000, time.UTC))
	log.Warn("day three written")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	msgs := func(lines []string) []string {
		var out []string
		for _, l := range lines {
			var v struct{ Msg string }
			if err := json.Unmarshal([]byte(l), &v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v.Msg)
		}
		return out
	}
	if got := rotatedOf(t, dir, "server"); !slices.Equal(got, []string{"server-2026-11-19.1.log.gz", "server-2026-11-20.1.log.gz"}) {
		t.Fatalf("server: %v", got)
	}
	server := filesOf(t, dir, "server")
	want := [][]string{{"day one written", "day one waiting"}, {"day two waiting"}, {"day three written"}}
	for i := range want {
		if got := msgs(server[i]); !slices.Equal(got, want[i]) {
			t.Errorf("server file %d: %q, want %q", i+1, got, want[i])
		}
	}
	for _, kind := range []string{"requests", "requests-classic"} {
		if got := rotatedOf(t, dir, kind); !slices.Equal(got, []string{kind + "-2026-11-19.1.log.gz"}) {
			t.Fatalf("%s: %v", kind, got)
		}
		files := filesOf(t, dir, kind)
		if len(files[0]) != 1 || !strings.Contains(files[0][0], "/day-one") || len(files[1]) != 0 {
			t.Errorf("%s: %q", kind, files)
		}
	}
	for _, kind := range []string{"audit", "tasks"} {
		if got := rotatedOf(t, dir, kind); got != nil {
			t.Errorf("%s rotated without a line: %v", kind, got)
		}
	}
}

// The next free number is one above the highest of the kind and the day
// among the .log and .log.gz names; a name of requests-classic never counts
// for requests, and a name of another day or kind not at all.
func TestRotationTakesTheNextFreeNumber(t *testing.T) {
	dir := t.TempDir()
	writeGzip(t, filepath.Join(dir, "requests-2026-11-19.1.log.gz"), []byte("{\"old\":1}\n"))
	if err := os.WriteFile(filepath.Join(dir, "requests-2026-11-19.2.log"), []byte("{\"old\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeGzip(t, filepath.Join(dir, "requests-classic-2026-11-19.7.log.gz"), []byte("old classic\n"))
	writeGzip(t, filepath.Join(dir, "requests-2026-11-18.9.log.gz"), []byte("{\"old\":9}\n"))
	writeGzip(t, filepath.Join(dir, "server-2026-11-19.4.log.gz"), []byte("{\"old\":4}\n"))
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: small(), Now: clk.now})
	const sent = 8
	for i := range sent {
		r := demoRequest("/numbered")
		r.ID = fmt.Sprintf("%016x", i)
		f.Request(r)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	requests := rotatedOf(t, dir, "requests")
	if len(requests) < 5 || requests[0] != "requests-2026-11-18.9.log.gz" || requests[1] != "requests-2026-11-19.1.log.gz" ||
		requests[2] != "requests-2026-11-19.2.log.gz" {
		t.Fatalf("requests: %v", requests)
	}
	for i, name := range requests[1:] {
		if want := fmt.Sprintf("requests-2026-11-19.%d.log.gz", i+1); name != want {
			t.Errorf("requests file %d is %s, want %s", i+1, name, want)
		}
	}
	if classic := rotatedOf(t, dir, "requests-classic"); !slices.Equal(classic, []string{"requests-classic-2026-11-19.7.log.gz", "requests-classic-2026-11-19.8.log.gz"}) {
		t.Fatalf("requests-classic: %v", classic)
	}
	if server := rotatedOf(t, dir, "server"); !slices.Equal(server, []string{"server-2026-11-19.4.log.gz"}) {
		t.Fatalf("server: %v", server)
	}
	if got := string(unzipped(t, filepath.Join(dir, "requests-2026-11-19.2.log.gz"))); got != "{\"old\":2}\n" {
		t.Fatalf("the leftover holds %q", got)
	}
	var ids []string
	for _, file := range filesOf(t, dir, "requests")[3:] {
		for _, line := range file {
			var v struct {
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, v.RequestID)
		}
	}
	if len(ids) != sent || ids[0] != fmt.Sprintf("%016x", 0) || ids[sent-1] != fmt.Sprintf("%016x", sent-1) {
		t.Fatalf("request ids %q", ids)
	}
}

// Eight writers and a ticker: across the rotations of server.log,
// requests.log and requests-classic.log every line reaches exactly one file.
// Each line carries its own number, and the numbers of each kind are the
// numbers sent, each once.
func TestRotationKeepsEveryLine(t *testing.T) {
	dir := t.TempDir()
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: small(), Now: clk.now})
	log := slog.New(f.Handler())
	const writers, each = 8, 60
	stop, ticked := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ticked)
		for {
			select {
			case <-stop:
				return
			default:
				f.Tick()
				runtime.Gosched()
			}
		}
	}()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				n := w*each + i
				if i%3 == 0 {
					log.Warn("line", "n", n)
				} else {
					log.Info("line", "n", n)
				}
				r := demoRequest("/numbered")
				r.ID = fmt.Sprintf("%016x", n)
				f.Request(r)
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-ticked
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	classicID := regexp.MustCompile(`\[([0-9a-f]{16})\]$`)
	number := map[string]func(line string) int{
		"server": func(line string) int { return numberOf(t, line) },
		"requests": func(line string) int {
			var v struct {
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				t.Fatalf("%q: %v", line, err)
			}
			n, err := strconv.ParseInt(v.RequestID, 16, 64)
			if err != nil {
				t.Fatalf("%q: %v", line, err)
			}
			return int(n)
		},
		"requests-classic": func(line string) int {
			m := classicID.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("no request id in %q", line)
			}
			n, _ := strconv.ParseInt(m[1], 16, 64)
			return int(n)
		},
	}
	for kind, numberOf := range number {
		files := filesOf(t, dir, kind)
		if len(files) < 10 {
			t.Errorf("%s: %d files", kind, len(files))
		}
		seen := make([]int, writers*each)
		count := 0
		for _, file := range files {
			for _, line := range file {
				n := numberOf(line)
				if n < 0 || n >= len(seen) {
					t.Fatalf("%s: number %d out of range", kind, n)
				}
				seen[n]++
				count++
			}
		}
		t.Logf("%s: %d lines in %d files", kind, count, len(files))
		if count != writers*each {
			t.Errorf("%s: %d lines, want %d", kind, count, writers*each)
		}
		for n, times := range seen {
			if times != 1 {
				t.Errorf("%s: line %d is in the files %d times", kind, n, times)
			}
		}
	}
	if f.Errors() != 0 {
		t.Fatalf("errors %d", f.Errors())
	}
}

// A limit of Settings that is not positive takes its default, so Open with
// zero Settings does not rotate at every line; Classic stays as given.
func TestZeroLimitsTakeTheDefaults(t *testing.T) {
	want := DefaultSettings()
	want.Classic = false
	for _, s := range []Settings{{}, {MaxBytes: -1, MaxAge: -time.Hour, MaxTotalBytes: -5}} {
		if got := s.withDefaults(); got != want {
			t.Errorf("%+v gives %+v, want %+v", s, got, want)
		}
	}
	if got := (Settings{MaxBytes: 7, MaxAge: time.Minute, MaxTotalBytes: 9, Classic: true}).withDefaults(); got.MaxBytes != 7 ||
		got.MaxAge != time.Minute || got.MaxTotalBytes != 9 || !got.Classic {
		t.Errorf("positive limits changed: %+v", got)
	}
	dir := t.TempDir()
	f := Open(Options{Dir: dir, Now: newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)).now})
	log := slog.New(f.Handler())
	log.Warn("first")
	log.Warn("second")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := rotatedOf(t, dir, "server"); got != nil {
		t.Fatalf("rotated with zero settings: %v", got)
	}
	if got := lines(t, dir, "server.log"); len(got) != 2 {
		t.Fatalf("server.log %q", got)
	}
}

// At Open an uncompressed rotated file of a crash is compressed, a half
// written .gz beside its .log is written again from the .log, and the
// temporary file of a compression that did not end is removed. An active
// file whose last write lies on an earlier UTC day rotates first, under that
// day; one written today is appended to. Names outside the pattern stay.
func TestOpenAfterACrash(t *testing.T) {
	dir := t.TempDir()
	put := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	left := "{\"msg\":\"left by a crash\"}\n"
	put("server-2026-11-17.1.log", left)
	audit := "{\"msg\":\"audit left\"}\n{\"msg\":\"second\"}\n"
	put("audit-2026-11-17.1.log", audit)
	var whole bytes.Buffer
	zw := gzip.NewWriter(&whole)
	_, _ = zw.Write([]byte(audit))
	_ = zw.Close()
	put("audit-2026-11-17.1.log.gz", string(whole.Bytes()[:whole.Len()/2]))
	put("requests-2026-11-17.3.log", "{\"msg\":\"requests left\"}\n")
	put("requests-2026-11-17.3.log.gz.tmp", "half")
	put("tasks-2026-11-17.2.log.gz.tmp", "half")
	put("server.log", "{\"msg\":\"yesterday one\"}\n{\"msg\":\"yesterday two\"}\n")
	if err := os.Chtimes(filepath.Join(dir, "server.log"), time.Time{}, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	put("requests.log", "{\"msg\":\"earlier today\"}\n")
	if err := os.Chtimes(filepath.Join(dir, "requests.log"), time.Time{}, time.Date(2026, 11, 19, 0, 30, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	foreign := []string{"notes.txt", "server.log.1", "server.log.gz", "server-2026-11-17.log", "server-2026-11-17.1.log.bak",
		"server-2026-11-17.0.log", "Server-2026-11-17.1.log", "server-2026-11-17.1.log.gz.old"}
	for _, name := range foreign {
		put(name, "not ours\n")
	}
	clk := newFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clk.now})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"server-2026-11-17.1.log.gz":   left,
		"audit-2026-11-17.1.log.gz":    audit,
		"requests-2026-11-17.3.log.gz": "{\"msg\":\"requests left\"}\n",
		"server-2026-11-18.1.log.gz":   "{\"msg\":\"yesterday one\"}\n{\"msg\":\"yesterday two\"}\n",
	} {
		path := filepath.Join(dir, name)
		gzipTest(t, path)
		if got := string(unzipped(t, path)); got != want {
			t.Errorf("%s holds %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"server-2026-11-17.1.log", "audit-2026-11-17.1.log", "requests-2026-11-17.3.log",
		"requests-2026-11-17.3.log.gz.tmp", "tasks-2026-11-17.2.log.gz.tmp", "tasks-2026-11-17.2.log.gz"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", name, err)
		}
	}
	if got := lines(t, dir, "server.log"); got != nil {
		t.Errorf("server.log after the rotation at Open: %q", got)
	}
	if got := lines(t, dir, "requests.log"); len(got) != 1 {
		t.Errorf("requests.log: %q", got)
	}
	if got := rotatedOf(t, dir, "requests"); !slices.Equal(got, []string{"requests-2026-11-17.3.log.gz"}) {
		t.Errorf("requests: %v", got)
	}
	for _, name := range foreign {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != "not ours\n" {
			t.Errorf("%s changed: %q %v", name, data, err)
		}
	}
}
