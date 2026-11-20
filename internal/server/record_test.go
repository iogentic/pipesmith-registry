package server

import (
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// writeStderrSample writes testdata/stderr-sample.txt from the access log of
// the sample requests with the sink off. The file in the repository was
// written once from the code of cf62b75:
//
//	tools/go.sh run 'go test ./internal/server -run "^TestStderrSample$" -write-stderr-sample'
var writeStderrSample = flag.Bool("write-stderr-sample", false, "write testdata/stderr-sample.txt from the sink off run")

const stderrSample = "testdata/stderr-sample.txt"

// recordSink collects the request records of one server.
type recordSink struct {
	mu    sync.Mutex
	list  []logs.Request
	taken int
}

func (rs *recordSink) add(r logs.Request) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.list = append(rs.list, r)
}

func (rs *recordSink) count() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.list)
}

// wait returns the records once n came; exactly n must have come.
func (rs *recordSink) wait(t *testing.T, n int) []logs.Request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for rs.count() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.list) != n {
		t.Fatalf("%d records, want %d: %+v", len(rs.list), n, rs.list)
	}
	return append([]logs.Request(nil), rs.list...)
}

// next is the record of the request just answered: exactly one new record
// came for it.
func (rs *recordSink) next(t *testing.T) logs.Request {
	t.Helper()
	rs.taken++
	return rs.wait(t, rs.taken)[rs.taken-1]
}

// collect turns the sink of the server of a on.
func collect(a *app) *recordSink {
	rs := &recordSink{}
	a.srv.requests = rs.add
	return rs
}

const recordJar = "jar bytes of the record test"

// recordApp serves demo-releases with the jar and demo-docker on a Docker
// port, with the sink on.
func recordApp(t *testing.T) (*app, *recordSink) {
	t.Helper()
	base := t.TempDir()
	blobs := append(blobsOf("demo-docker", dockerImage("demo/app", "1.0", []byte("layer one"))), jarSpec(recordJar))
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1", blobs...)}, appOptions{
		defs:        []*pyjson.Object{def("demo-releases", "maven2", "hosted"), dockerDef("demo-docker", 14760)},
		dockerRepos: []string{"demo-docker"},
	})
	return a, collect(a)
}

var requestID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// common checks what every record of TestRecordPerRequest holds.
func common(t *testing.T, rec logs.Request, before, after time.Time) {
	t.Helper()
	if !requestID.MatchString(rec.ID) {
		t.Errorf("id %q", rec.ID)
	}
	if rec.Time.Location() != time.UTC || rec.Time.Before(before.Add(-time.Second)) || rec.Time.After(after.Add(time.Second)) {
		t.Errorf("time %v, want UTC between %v and %v", rec.Time, before, after)
	}
	if rec.Duration <= 0 || rec.Duration > after.Sub(before)+time.Second {
		t.Errorf("duration %v", rec.Duration)
	}
	if rec.Protocol != "HTTP/1.1" {
		t.Errorf("protocol %q", rec.Protocol)
	}
}

// One record per request, with the status, the sizes, the path and the
// client the access line names.
func TestRecordPerRequest(t *testing.T) {
	a, rs := recordApp(t)
	mainPort := a.ts.Listener.Addr().(*net.TCPAddr).Port
	scanBody := []byte(`{"store": "demo-none"}`)
	putBody := []byte(strings.Repeat("u", 1000))
	type answer struct {
		status int
		body   []byte
	}
	cases := []struct {
		name   string
		send   func() answer
		status int
		check  func(t *testing.T, rec logs.Request)
	}{
		{"GET of a file", func() answer {
			r := a.get(jarURL, "User-Agent", "demo-agent/1.0")
			return answer{r.status, r.body}
		}, 200, func(t *testing.T, rec logs.Request) {
			if rec.Method != "GET" || rec.Path != jarURL || rec.Query != "" || rec.UserAgent != "demo-agent/1.0" ||
				rec.Client != "127.0.0.1" || rec.ContentLength != "" || rec.RequestBytes != 0 || rec.Port != mainPort {
				t.Errorf("%+v", rec)
			}
			if rec.ResponseBytes != int64(len(recordJar)) {
				t.Errorf("response bytes %d, want %d", rec.ResponseBytes, len(recordJar))
			}
		}},
		{"HEAD of a file", func() answer { r := a.head(jarURL); return answer{r.status, r.body} }, 200,
			func(t *testing.T, rec logs.Request) {
				if rec.Method != "HEAD" || rec.ResponseBytes != 0 || rec.UserAgent != "" {
					t.Errorf("%+v", rec)
				}
			}},
		{"404", func() answer { r := a.get("/repository/demo-releases/missing.jar"); return answer{r.status, r.body} }, 404, nil},
		{"redirect", func() answer { r := a.get("/api/health/"); return answer{r.status, r.body} }, 307, nil},
		{"405", func() answer { r := raw(t, a.addr(), "DELETE", "/api/health"); return answer{r.status, r.body} }, 405, nil},
		{"host guard", func() answer { r := a.get("/api/health", "Host", "example.com"); return answer{r.status, r.body} }, 403,
			func(t *testing.T, rec logs.Request) {
				if rec.Path != "/api/health" || rec.Client != "127.0.0.1" {
					t.Errorf("%+v", rec)
				}
			}},
		{"query string", func() answer {
			r := a.get("/repository/demo-releases/a%20b?x=%20&y=1")
			return answer{r.status, r.body}
		}, 404, func(t *testing.T, rec logs.Request) {
			if rec.Path != "/repository/demo-releases/a%20b" || rec.Query != "x=%20&y=1" {
				t.Errorf("path %q query %q", rec.Path, rec.Query)
			}
		}},
		{"forwarded client", func() answer {
			r := a.get("/api/health", "X-Forwarded-For", "203.0.113.9")
			return answer{r.status, r.body}
		}, 200, func(t *testing.T, rec logs.Request) {
			if rec.Client != "203.0.113.9" {
				t.Errorf("client %q", rec.Client)
			}
		}},
		{"forwarded IPv6 client", func() answer {
			r := a.get("/api/health", "X-Forwarded-For", "[2001:db8::1]:443")
			return answer{r.status, r.body}
		}, 200, func(t *testing.T, rec logs.Request) {
			if rec.Client != "2001:db8::1" {
				t.Errorf("client %q", rec.Client)
			}
		}},
		{"Docker port", func() answer { r := a.on("demo-docker", "GET", "/v2/"); return answer{r.status, r.body} }, 200,
			func(t *testing.T, rec logs.Request) {
				port := a.docker["demo-docker"].Listener.Addr().(*net.TCPAddr).Port
				if rec.Path != "/v2/" || rec.Port != port || rec.Repository != "demo-docker" || rec.Format != "docker" {
					t.Errorf("%+v, want port %d", rec, port)
				}
			}},
		{"invalid request", func() answer {
			r := a.get("/repository/demo-releases/\xc3\xa4?demo=x")
			return answer{r.status, r.body}
		}, 400, func(t *testing.T, rec logs.Request) {
			if rec.Path != "/repository/demo-releases/\xc3\xa4" || rec.Query != "demo=x" || rec.Client != "127.0.0.1" ||
				rec.ResponseBytes != 30 {
				t.Errorf("%+v", rec)
			}
		}},
		{"body read by the handler", func() answer {
			r := do(t, a.addr(), "POST", "/api/scan", scanBody, postHeader, "1")
			return answer{r.status, r.body}
		}, 404, func(t *testing.T, rec logs.Request) {
			if rec.RequestBytes != int64(len(scanBody)) || rec.ContentLength != strconv.Itoa(len(scanBody)) {
				t.Errorf("request bytes %d content length %q, want %d", rec.RequestBytes, rec.ContentLength, len(scanBody))
			}
		}},
		{"body the handler leaves", func() answer {
			r := do(t, a.addr(), "PUT", "/repository/demo-releases/upload.jar", putBody)
			return answer{r.status, r.body}
		}, 405, func(t *testing.T, rec logs.Request) {
			if rec.RequestBytes != 0 || rec.ContentLength != "1000" || rec.Method != "PUT" {
				t.Errorf("request bytes %d content length %q", rec.RequestBytes, rec.ContentLength)
			}
		}},
	}
	ids := map[string]bool{}
	for _, c := range cases {
		before := time.Now()
		got := c.send()
		after := time.Now()
		if got.status != c.status {
			t.Fatalf("%s: status %d, want %d: %q", c.name, got.status, c.status, got.body)
		}
		rec := rs.next(t)
		common(t, rec, before, after)
		if rec.Status != c.status {
			t.Errorf("%s: record status %d, want %d", c.name, rec.Status, c.status)
		}
		if rec.ResponseBytes != int64(len(got.body)) {
			t.Errorf("%s: response bytes %d, the client received %d", c.name, rec.ResponseBytes, len(got.body))
		}
		if ids[rec.ID] {
			t.Errorf("%s: id %s again", c.name, rec.ID)
		}
		ids[rec.ID] = true
		if c.check != nil {
			c.check(t, rec)
		}
	}
	if len(ids) != len(cases) {
		t.Fatalf("%d ids for %d requests", len(ids), len(cases))
	}
}

// Repository and format: the repository after /repository/, after the two
// browse prefixes and of a Docker port, empty for one that does not exist.
func TestRecordRepository(t *testing.T) {
	a, rs := recordApp(t)
	for _, c := range []struct {
		target, repository, format string
	}{
		{jarURL, "demo-releases", "maven2"},
		{"/repository/demo-releases", "demo-releases", "maven2"},
		{"/service/rest/repository/browse/demo-releases/com/", "demo-releases", "maven2"},
		{"/api/browse/demo-releases", "demo-releases", "maven2"},
		{"/repository/demo-docker/v2/", "demo-docker", "docker"},
		{"/repository/demo-missing/a.txt", "", ""},
		{"/api/browse/demo-missing", "", ""},
		{"/api/health", "", ""},
	} {
		a.get(c.target)
		rec := rs.next(t)
		if rec.Repository != c.repository || rec.Format != c.format {
			t.Errorf("%s: repository %q format %q, want %q %q", c.target, rec.Repository, rec.Format, c.repository, c.format)
		}
	}
	a.on("demo-docker", "GET", "/api/health")
	if rec := rs.next(t); rec.Repository != "demo-docker" || rec.Format != "docker" {
		t.Errorf("Docker port: %+v", rec)
	}
}

// With security on: a signed in request carries subject and realm, a public
// route and an anonymous request carry none.
func TestRecordSubject(t *testing.T) {
	sec := newFakeSecurity()
	sec.anonymous = true
	sec.anonymousPermissions = []string{"*"}
	admin := sec.basic[asAdmin]
	s := newSecureApp(t, sec)
	rs := collect(s.app)
	file := "/repository/demo-maven/" + jarPath
	status(t, s.send("GET", file, "basic", asAdmin), 200)
	if rec := rs.next(t); rec.Subject != "demo-admin" || rec.Realm != admin.Realm || rec.Realm == "" {
		t.Errorf("signed in: subject %q realm %q, want demo-admin %q", rec.Subject, rec.Realm, admin.Realm)
	}
	status(t, s.send("GET", "/api/health", "basic", asAdmin), 200)
	if rec := rs.next(t); rec.Subject != "" || rec.Realm != "" {
		t.Errorf("public route: subject %q realm %q", rec.Subject, rec.Realm)
	}
	status(t, s.send("GET", file), 200)
	if rec := rs.next(t); rec.Subject != "" || rec.Realm != "" {
		t.Errorf("anonymous: subject %q realm %q", rec.Subject, rec.Realm)
	}
}

// seenSecurity is the fake of secure_test.go whose session handler keeps the
// writer and the request it is handed.
type seenSecurity struct {
	*fakeSecurity
	w http.ResponseWriter
	r *http.Request
}

func (s *seenSecurity) SessionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.w, s.r = w, r
		s.fakeSecurity.SessionHandler().ServeHTTP(w, r)
	})
}

// With the sink off the handler sees the very writer and request ServeHTTP
// was given; with the sink on both are wrapped.
func TestNoSinkWrapsNothing(t *testing.T) {
	sec := &seenSecurity{fakeSecurity: newFakeSecurity()}
	a := startSecured(t, sec)
	serve := func() (http.ResponseWriter, *http.Request) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/session", nil)
		r.Host = "localhost"
		a.srv.ServeHTTP(w, r)
		if w.Code != 299 {
			t.Fatalf("status %d", w.Code)
		}
		return w, r
	}
	w, r := serve()
	if inner := sec.w.(*recorder).ResponseWriter; inner != w || sec.r != r {
		t.Fatalf("sink off: the handler got %T and another request", inner)
	}
	rs := collect(a)
	w, r = serve()
	if inner := sec.w.(*recorder).ResponseWriter; inner == w || sec.r == r {
		t.Fatal("sink on: writer and request are not wrapped")
	}
	if rec := rs.next(t); rec.Status != 299 || rec.ResponseBytes != int64(len(`{"fake":"session"}`)) {
		t.Fatalf("%+v", rec)
	}
}

// The body count is what the handler read, the status the first one, and
// the writer unwraps for http.ResponseController.
func TestTrackCountsWhatWasRead(t *testing.T) {
	rs := &recordSink{}
	s := &Server{requests: rs.add}
	recorded := httptest.NewRecorder()
	given := httptest.NewRequest("PUT", "/demo?a=1", strings.NewReader(strings.Repeat("b", 100)))
	w, r := http.ResponseWriter(recorded), given
	done := s.track(&w, &r)
	if _, err := io.ReadFull(r.Body, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	w.WriteHeader(http.StatusNoContent)
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil || !recorded.Flushed {
		t.Fatalf("flush through the wrapper: %v", err)
	}
	done()
	rec := rs.next(t)
	if rec.RequestBytes != 10 || rec.Status != http.StatusNoContent || rec.Path != "/demo" || rec.Query != "a=1" ||
		rec.Client != "192.0.2.1" || rec.Port != 0 || rec.ContentLength != "" {
		t.Fatalf("%+v", rec)
	}
	off := &Server{}
	w, r = http.ResponseWriter(recorded), given
	off.track(&w, &r)()
	if w != http.ResponseWriter(recorded) || r != given {
		t.Fatal("without a sink track replaced writer or request")
	}
}

// sampleRequests are the requests of the standard error sample: GET and HEAD
// of a file, a 404, a redirect, a 405, a query string, a forwarded client,
// the host guard and the invalid request warning.
func sampleRequests(a *app) int {
	a.get(jarURL)
	a.head(jarURL)
	a.get("/repository/demo-releases/missing.jar")
	a.get("/api/health/")
	raw(a.t, a.addr(), "DELETE", "/api/health")
	a.get("/repository/demo-releases/a%20b?x=%20&y=1")
	a.get("/api/health", "X-Forwarded-For", "203.0.113.9")
	a.get("/api/health", "Host", "example.com")
	a.get("/repository/demo-releases/\xc3\xa4")
	return 9
}

// loopbackPort is the port of a loopback client in an access line.
var loopbackPort = regexp.MustCompile(`(?m)^(INFO:     127\.0\.0\.1):\d+ `)

// sampleLog sends the sample requests to a new server and returns its access
// log with the port of a loopback client written as <port>.
func sampleLog(t *testing.T, sink bool) string {
	t.Helper()
	a, _ := jarApp(t, "jar bytes of the sample")
	var rs *recordSink
	if sink {
		rs = collect(a)
	}
	sent := sampleRequests(a)
	if sink {
		rs.wait(t, sent)
	}
	return loopbackPort.ReplaceAllString(a.access.String(), "$1:<port> ")
}

// TestStderrSample: the access lines on standard error equal the stored
// sample of the port byte for byte, with the sink off and with it on.
func TestStderrSample(t *testing.T) {
	off := sampleLog(t, false)
	if *writeStderrSample {
		if err := os.WriteFile(stderrSample, []byte(off), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := string(mustRead(t, stderrSample))
	if off != want {
		t.Fatalf("sink off:\n%s\nwant\n%s", off, want)
	}
	if on := sampleLog(t, true); on != want {
		t.Fatalf("sink on:\n%s\nwant\n%s", on, want)
	}
}
